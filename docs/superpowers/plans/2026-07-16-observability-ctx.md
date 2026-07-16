# P2 可观测性 + ctx 响应 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 serve 模式增加运行期可观测性（`/health`+`/downloads/{id}` 健康字段、错误码分级日志+内存计数）并让 `tsparser`/`mergeTSFiles` 响应 ctx 及时退出。

**Architecture:** 新增 `job.Stats`（4 桶 atomic 计数）由 worker 持有并在终态错误处自增+分级日志；worker 暴露 `Stats/LastActiveAt/Uptime/RateLimitUntil`；`/health`、`/downloads/{id}` 组装新字段；`tsparser`/`mergeTSFiles` 加 ctx 在 PES/分段边界检查。无 DB/config/CLI 变更。

**Tech Stack:** Go 1.22+、`net/http`+`httptest`、logrus、标准库 `testing`。

## Global Constraints

- P0+P1 已合并进 `main`；本计划在分支 `feat/p2-observability-ctx` 上。
- Go module `github.com/nicoxiang/geektime-downloader`；测试 `go test ./internal/...`；构建 `go build ./...`。
- 错误码计数**内存**，不持久化（YAGNI）。
- `parseTS` 的 packet 循环**不加** ctx 检查；仅 PES 边界与 mergeTSFiles 分段边界检查。
- 无新 config flag、无 schema 变更、无 CLI 行为变更。
- `apperr` code 常量：`AUTH_EXPIRED`/`RATE_LIMITED`/`TIMEOUT`/`INTERNAL_ERROR` 等（见 `internal/apperr/errors.go`）。
- 每个任务结束 `go build ./...` + 相关包 `go test` 通过后提交；conventional commit 风格。

---

## File Structure

| 文件 | 责任 | 任务 |
|------|------|------|
| `internal/job/stats.go` | `Stats` 4 桶 atomic 计数 + `Inc`/`Snapshot` | T1 |
| `internal/job/stats_test.go` | Stats 并发计数测试 | T1 |
| `internal/job/store.go` | `CountStaleJobs` | T2 |
| `internal/job/store_test.go` | `CountStaleJobs` 测试 | T2 |
| `internal/m3u8/tsparser.go` | `NewTSParser`/`Decrypt`/`decryptPES` 加 ctx | T3 |
| `internal/m3u8/tsparser_test.go` | ctx 取消测试 | T3 |
| `internal/video/video.go` | `mergeTSFiles` 加 ctx；调用补 ctx | T4 |
| `internal/video/video_test.go` | mergeTSFiles ctx 测试 | T4 |
| `internal/job/worker.go` | 持有 stats/lastActiveAt/startedAt；分级日志；暴露 Stats/LastActiveAt/Uptime/RateLimitUntil | T5 |
| `internal/job/worker_test.go` | NewWorker 调用补 nil | T5 |
| `cmd/serve.go` | 注入 `&job.Stats{}` | T5 |
| `internal/job/model.go` | `Job.RuntimeSeconds` | T6 |
| `internal/api/handlers.go` | handleHealth/handleGetDownload 新字段 | T6 |
| `internal/api/handlers_test.go` | health/get 测试 | T6 |

---

### Task 1: job.Stats — 错误码内存计数器

**Files:**
- Create: `internal/job/stats.go`
- Test: `internal/job/stats_test.go`

**Interfaces:**
- Produces: `job.Stats{}`（零值可用）；`(s *Stats) Inc(code string)`；`(s *Stats) Snapshot() map[string]int64`（key: `AUTH_EXPIRED`/`RATE_LIMITED`/`TIMEOUT`/`INTERNAL_ERROR`）。未识别 code 归入 `INTERNAL_ERROR`。

- [ ] **Step 1: Write failing test**

新建 `internal/job/stats_test.go`：

```go
package job_test

import (
	"sync"
	"testing"

	"github.com/nicoxiang/geektime-downloader/internal/job"
)

func TestStatsIncAndSnapshot(t *testing.T) {
	var s job.Stats
	s.Inc("AUTH_EXPIRED")
	s.Inc("AUTH_EXPIRED")
	s.Inc("RATE_LIMITED")
	s.Inc("TIMEOUT")
	s.Inc("SOMETHING_UNKNOWN") // -> INTERNAL_ERROR

	got := s.Snapshot()
	if got["AUTH_EXPIRED"] != 2 {
		t.Fatalf("AUTH_EXPIRED want 2, got %d", got["AUTH_EXPIRED"])
	}
	if got["RATE_LIMITED"] != 1 {
		t.Fatalf("RATE_LIMITED want 1, got %d", got["RATE_LIMITED"])
	}
	if got["TIMEOUT"] != 1 {
		t.Fatalf("TIMEOUT want 1, got %d", got["TIMEOUT"])
	}
	if got["INTERNAL_ERROR"] != 1 {
		t.Fatalf("INTERNAL_ERROR want 1, got %d", got["INTERNAL_ERROR"])
	}
}

func TestStatsConcurrent(t *testing.T) {
	var s job.Stats
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Inc("AUTH_EXPIRED")
			s.Inc("RATE_LIMITED")
		}()
	}
	wg.Wait()
	got := s.Snapshot()
	if got["AUTH_EXPIRED"] != 100 || got["RATE_LIMITED"] != 100 {
		t.Fatalf("want 100/100, got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/job/ -run TestStats -v`
Expected: FAIL（`job.Stats` / `Inc` / `Snapshot` 未定义）

- [ ] **Step 3: Implement**

新建 `internal/job/stats.go`：

```go
package job

import "sync/atomic"

// Stats counts terminal job errors by apperr code. In-memory, resets on restart.
// Zero value is ready to use. Unknown codes roll up into INTERNAL_ERROR.
type Stats struct {
	authExpired, rateLimited, timeout, internalErr atomic.Int64
}

// Inc increments the counter for the given apperr code.
func (s *Stats) Inc(code string) {
	switch code {
	case "AUTH_EXPIRED":
		s.authExpired.Add(1)
	case "RATE_LIMITED":
		s.rateLimited.Add(1)
	case "TIMEOUT":
		s.timeout.Add(1)
	default:
		s.internalErr.Add(1)
	}
}

// Snapshot returns a copy of current counts keyed by apperr code.
func (s *Stats) Snapshot() map[string]int64 {
	return map[string]int64{
		"AUTH_EXPIRED":   s.authExpired.Load(),
		"RATE_LIMITED":   s.rateLimited.Load(),
		"TIMEOUT":        s.timeout.Load(),
		"INTERNAL_ERROR": s.internalErr.Load(),
	}
}
```

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/job/ -run TestStats -v && go build ./...`
Expected: PASS；`-race` clean：`go test -race ./internal/job/ -run TestStats`

- [ ] **Step 5: Commit**

```bash
git add internal/job/stats.go internal/job/stats_test.go
git commit -m "feat(job): add Stats in-memory error-code counter"
```

---

### Task 2: job/store — CountStaleJobs

**Files:**
- Modify: `internal/job/store.go`
- Modify: `internal/job/store_test.go`

**Interfaces:**
- Produces: `(*Store).CountStaleJobs(ctx context.Context) (int64, error)`（`SELECT count(*) FROM jobs WHERE status='running' AND status_reason='stale_progress'`）。

- [ ] **Step 1: Write failing test**

追加到 `internal/job/store_test.go`（复用已有的 `newTestStore` helper）：

```go
func TestCountStaleJobs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// running but NOT stale -> count 0
	_ = store.UpdateJobStatus(ctx, j.ID, job.StatusRunning, "", nil)
	n, err := store.CountStaleJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want 0 stale, got %d", n)
	}
	// mark stale
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := store.DB().ExecContext(ctx, `UPDATE jobs SET status_reason='stale_progress', updated_at=? WHERE id=?`, past, j.ID); err != nil {
		t.Fatal(err)
	}
	n, err = store.CountStaleJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 stale, got %d", n)
	}
}
```

> 实现者：`store_test.go` 是 `package job_test`，已有 `context`/`time`/`job`/`service` import；确认齐全。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/job/ -run TestCountStaleJobs -v`
Expected: FAIL（`CountStaleJobs` 未定义）

- [ ] **Step 3: Implement**

在 `internal/job/store.go` 的 `MarkStaleJobs` 后追加：

```go
// CountStaleJobs returns the number of running jobs flagged stale_progress.
func (s *Store) CountStaleJobs(ctx context.Context) (int64, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM jobs
		WHERE status = ? AND status_reason = ?
	`, StatusRunning, "stale_progress")
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
```

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/job/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/job/store.go internal/job/store_test.go
git commit -m "feat(job): add Store.CountStaleJobs"
```

---

### Task 3: m3u8/tsparser — ctx 响应

**Files:**
- Modify: `internal/m3u8/tsparser.go`
- Test: `internal/m3u8/tsparser_test.go`（新建）

**Interfaces:**
- Produces: `NewTSParser(ctx context.Context, data []byte, key string) (*TSParser, error)`；`(*TSParser) Decrypt(ctx context.Context) ([]byte, error)`；`(*TSParser) decryptPES(ctx context.Context, ...) error`。`decryptPES` 的 `for _, pes := range pesFragments` 循环顶部 `ctx.Err()` 检查。
- Consumes: 无（`parseTS` 不改）。

> 注：`tsparser.go` 现有调用点在 `internal/video/video.go`（`m3u8.NewTSParser(f, string(key))` 与 `tsParser.Decrypt()`）——本任务只改 tsparser 签名；video 调用点的修复在 T4（签名变更会导致 video 编译失败，T4 紧接修复，故 T3 提交时 `go build ./...` 会失败，仅 `go test ./internal/m3u8/` 通过即可；T4 结束恢复全量绿）。或者：T3 先不改 video 调用、用 build tag？不——按计划 T3 只跑 m3u8 包测试 + 自身构建，T4 立即修 video。`go build ./...` 在 T3 提交点会失败，这是可接受的中间态；T3 Step 4 改为 `go test ./internal/m3u8/ -v`（不强求全量 build）。

- [ ] **Step 1: Write failing test**

新建 `internal/m3u8/tsparser_test.go`：

```go
package m3u8

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// makeEncryptedTS builds a TS stream of numPackets packets, each 188 bytes,
// with video PID 0x100 payload encrypted AES-ECB. Returns stream bytes + hex key.
func makeEncryptedTS(t *testing.T, numPackets int) ([]byte, string) {
	t.Helper()
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	stream := cipher.NewECBEncrypter(block)
	data := make([]byte, numPackets*188)
	for p := 0; p < numPackets; p++ {
		off := p * 188
		data[off] = 0x47 // syncByte
		// PID 0x100, payload_start, payload only (adaptationField=01)
		data[off+1] = 0x40 // payload_unit_start=1, pid high = 0x00
		data[off+2] = 0x00 // pid low -> 0x000 -> set below
		// set pid 0x100: buffer[1]&0x1F<<8 | buffer[2] = 0x100
		data[off+1] |= 0x10 // 0x10<<8? pid high nibble
		data[off+2] = 0x00
		data[off+3] = 0x10 // adaptationField=01 (payload only), cc=0
		// mark as payload start for the FIRST packet of each fragment only;
		// to keep it simple, every packet is a payload start -> each is its own fragment.
		data[off+1] |= 0x40 // payloadUnitStartIndicator
		payload := data[off+4 : off+188]
		// encrypt payload in 16-byte blocks (ECB)
		for i := 0; i+16 <= len(payload); i += 16 {
			stream.Encrypt(payload[i:i+16], payload[i:i+16])
		}
	}
	return data, hex.EncodeToString(key)
}

func TestTSParserDecryptCancelledByCtx(t *testing.T) {
	data, key := makeEncryptedTS(t, 20000) // large enough that decryptPES loops
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	p, err := NewTSParser(ctx, data, key)
	if err != nil {
		// parseTS doesn't check ctx; parser may still build ok. That's fine.
		p, err = NewTSParser(context.Background(), data, key)
		if err != nil {
			t.Skip("ts stream construction unsupported in test; skip")
		}
	}
	_, err = p.Decrypt(ctx)
	if err == nil {
		t.Fatal("Decrypt with cancelled ctx should return ctx.Err(), got nil")
	}
}

func TestTSParserDecryptNormalCtx(t *testing.T) {
	data, key := makeEncryptedTS(t, 100)
	p, err := NewTSParser(context.Background(), data, key)
	if err != nil {
		t.Fatalf("NewTSParser: %v", err)
	}
	if _, err := p.Decrypt(context.Background()); err != nil {
		t.Fatalf("Decrypt with normal ctx should succeed, got %v", err)
	}
}
```

> 实现者：若 `makeEncryptedTS` 构造的流 `parseTS` 报错（PID/adaptation 编码不合法），调整 helper 使其能被 `parseTS` 接受；测试核心断言是“ctx 已取消时 `Decrypt` 返回非 nil error”。若无法稳定构造合法 ts 流，退化为：直接构造一个 `*tsStream`（同包测试可访问未导出字段）填入多个 `pesFragments`，断言 `decryptPES(ctxCancelled, ...)` 返回 `ctx.Err()`。优先用此退化方案保证测试稳定。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/m3u8/ -v`
Expected: FAIL（`NewTSParser`/`Decrypt` 签名不匹配 ctx）

- [ ] **Step 3: Implement**

`internal/m3u8/tsparser.go`：

`NewTSParser` 签名加 ctx：
```go
func NewTSParser(ctx context.Context, data []byte, key string) (*TSParser, error) {
	hexKey, err := hex.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("decode key hex failed: %w", err)
	}
	stream := &tsStream{
		data: data,
		key:  hexKey,
	}
	if err := stream.parseTS(); err != nil {
		return nil, err
	}
	return &TSParser{stream: stream}, nil
}
```
> `ctx` 在 `NewTSParser` 内未使用（`parseTS` 不改），仅为签名统一与调用方透传。保留参数。

`Decrypt` 签名加 ctx：
```go
func (p *TSParser) Decrypt(ctx context.Context) ([]byte, error) {
	if err := p.decryptPES(ctx, p.stream.data, p.stream.videos, p.stream.key); err != nil {
		return nil, err
	}
	if err := p.decryptPES(ctx, p.stream.data, p.stream.audios, p.stream.key); err != nil {
		return nil, err
	}
	return p.stream.data, nil
}
```

`decryptPES` 签名加 ctx，循环顶部检查：
```go
func (p *TSParser) decryptPES(ctx context.Context, byteBuf []byte, pesFragments []*tsPesFragment, key []byte) error {
	for _, pes := range pesFragments {
		if err := ctx.Err(); err != nil {
			return err
		}
		// ... existing body unchanged ...
	}
	return nil
}
```

import 块补 `"context"`。

- [ ] **Step 4: Run m3u8 tests**

Run: `go test ./internal/m3u8/ -v`
Expected: PASS（`go build ./...` 此时因 video 调用点未改会失败——T4 修复，本步不强求全量 build）

- [ ] **Step 5: Commit**

```bash
git add internal/m3u8/tsparser.go internal/m3u8/tsparser_test.go
git commit -m "feat(m3u8): make tsparser ctx-aware (PES boundary checks)"
```

---

### Task 4: video — mergeTSFiles 响应 ctx + 调用补 ctx

**Files:**
- Modify: `internal/video/video.go`
- Test: `internal/video/video_test.go`

**Interfaces:**
- Produces: `mergeTSFiles(ctx context.Context, tempVideoDir, filenamifyTitle, projectDir string, key []byte, isVodEncryptVideo bool) error`；循环每段顶部 `ctx.Err()` 检查；`NewTSParser`/`Decrypt` 调用补 ctx。
- Consumes: T3 的 `m3u8.NewTSParser(ctx, data, key)` / `(*TSParser).Decrypt(ctx)`。

- [ ] **Step 1: Write failing test**

追加到 `internal/video/video_test.go`（包 `video`，内部测试可访问 `mergeTSFiles`）：

```go
func TestMergeTSFilesCtxCancelled(t *testing.T) {
	tempDir := t.TempDir()
	// 2 small fake ts files (non-encrypted path, no decrypt)
	for _, n := range []string{"a.ts", "b.ts"} {
		if err := os.WriteFile(filepath.Join(tempDir, n), []byte("x"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	projectDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mergeTSFiles(ctx, tempDir, "title", projectDir, nil, false)
	if err == nil {
		t.Fatal("want ctx.Err() from cancelled ctx, got nil")
	}
	// final video file must not be left behind on error
	if _, statErr := os.Stat(filepath.Join(projectDir, "title.ts")); statErr == nil {
		t.Fatal("final video file should be removed on ctx error")
	}
}
```

> import 块补 `"context"`、`"os"`、`"path/filepath"`（按需）。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/video/ -run TestMergeTSFilesCtxCancelled -v`
Expected: FAIL（`mergeTSFiles` 无 ctx 参数，编译错误）

- [ ] **Step 3: Implement**

`internal/video/video.go` 的 `mergeTSFiles` 签名与循环：

```go
func mergeTSFiles(ctx context.Context, tempVideoDir, filenamifyTitle, projectDir string, key []byte, isVodEncryptVideo bool) error {
	tempTSFiles, err := os.ReadDir(tempVideoDir)
	if err != nil {
		return err
	}
	fullPath := filepath.Join(projectDir, filenamifyTitle+TSExtension)
	finalVideoFile, err := os.OpenFile(fullPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, os.ModePerm)
	if err != nil {
		return err
	}
	removeOnError := false
	defer func() {
		_ = finalVideoFile.Close()
		if removeOnError {
			_ = os.Remove(fullPath)
		}
	}()
	for _, tempTSFile := range tempTSFiles {
		if err := ctx.Err(); err != nil {
			removeOnError = true
			return err
		}
		f, err := os.ReadFile(filepath.Join(tempVideoDir, tempTSFile.Name()))
		if err != nil {
			removeOnError = true
			return err
		}
		if isVodEncryptVideo {
			tsParser, err := m3u8.NewTSParser(ctx, f, string(key))
			if err != nil {
				removeOnError = true
				return err
			}
			f, err = tsParser.Decrypt(ctx)
			if err != nil {
				removeOnError = true
				return err
			}
		}
		if _, err := finalVideoFile.Write(f); err != nil {
			removeOnError = true
			return err
		}
	}
	return nil
}
```

`download` 函数内调用处补 ctx：
```go
	err = mergeTSFiles(ctx, tempVideoDir, filenamifyTitle, projectDir, decryptKey, isVodEncryptVideo)
```

import 块补 `"context"`（若未导入）。

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/video/ -v && go build ./...`
Expected: PASS；全量构建恢复绿（T3 的 tsparser 签名现在被 video 正确调用）。

- [ ] **Step 5: Commit**

```bash
git add internal/video/video.go internal/video/video_test.go
git commit -m "feat(video): make mergeTSFiles ctx-aware and thread ctx to tsparser"
```

---

### Task 5: job/worker — 持有 Stats + 分级日志 + 暴露方法

**Files:**
- Modify: `internal/job/worker.go`
- Modify: `internal/job/worker_test.go`（现有 `NewWorker` 调用补 `nil` 第 5 参）
- Modify: `cmd/serve.go`（注入 `&job.Stats{}`）
- Test: `internal/job/worker_test.go`（新增 `TestWorkerStatsAndExposure`）

**Interfaces:**
- Produces: `NewWorker(store *Store, exec executor, clientProvider func() *geektime.Client, st Stability, stats *Stats)`；`(*Worker).Stats() map[string]int64`、`LastActiveAt() time.Time`、`Uptime() time.Duration`、`RateLimitUntil() time.Time`；`runJob` 终态错误处 `recordTerminal(jobID, apiErr)`（分级日志 + `stats.Inc`）。
- Consumes: T1 `job.Stats`。

- [ ] **Step 1: Write failing test**

追加到 `internal/job/worker_test.go`：

```go
func TestWorkerStatsAndExposure(t *testing.T) {
	store := newTestStoreTB(t)
	stats := &Stats{}
	w := NewWorker(store, &fakeExecutor{err: context.DeadlineExceeded}, nil, Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: time.Minute,
	}, stats)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	w.runJob(ctx, j.ID)

	snap := w.Stats()
	if snap["TIMEOUT"] != 1 {
		t.Fatalf("want TIMEOUT=1, got %v", snap)
	}
	if w.Uptime() < 0 {
		t.Fatal("uptime should be non-negative")
	}
	if w.RateLimitUntil().IsZero() {
		// no rate limit applied in this path -> zero is fine
	}
	_ = w.LastActiveAt() // must not panic
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/job/ -run TestWorkerStatsAndExposure -v`
Expected: FAIL（`NewWorker` 无第 5 参 `stats`，`Stats()`/`Uptime()`/`LastActiveAt()` 未定义）

- [ ] **Step 3: Implement worker.go**

`Worker` struct 加字段：
```go
type Worker struct {
	store          *Store
	exec           executor
	clientProvider func() *geektime.Client
	stability      Stability
	stats          *Stats
	startedAt      time.Time
	lastActiveAt   atomic.Int64
	pausedAuth     atomic.Bool

	mu          sync.Mutex
	activeJobID string
	cancel      context.CancelFunc
	notify      chan struct{}
}
```

`NewWorker` 加 `stats *Stats` 参数与 `startedAt`：
```go
func NewWorker(store *Store, exec executor, clientProvider func() *geektime.Client, st Stability, stats *Stats) *Worker {
	return &Worker{
		store: store, exec: exec, clientProvider: clientProvider,
		stability: st, stats: stats, startedAt: time.Now(),
		notify: make(chan struct{}, 1),
	}
}
```

新增暴露方法与 `recordTerminal`/`touchActive`：
```go
func (w *Worker) Stats() map[string]int64 {
	if w.stats == nil {
		return nil
	}
	return w.stats.Snapshot()
}

func (w *Worker) LastActiveAt() time.Time {
	n := w.lastActiveAt.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (w *Worker) Uptime() time.Duration {
	return time.Since(w.startedAt)
}

func (w *Worker) RateLimitUntil() time.Time {
	n := w.rateLimitUntil.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (w *Worker) touchActive() {
	w.lastActiveAt.Store(time.Now().UnixNano())
}

// recordTerminal logs + counts a terminal job error by apperr code.
// Cancelled (user intent) is not logged/counted as an error.
func (w *Worker) recordTerminal(jobID string, apiErr *apperr.APIError) {
	if apiErr == nil || apiErr.Code == apperr.CodeCancelled {
		return
	}
	switch apiErr.Code {
	case apperr.CodeAuthExpired, apperr.CodeRateLimited, apperr.CodeTimeout:
		logger.Warnf("job %s terminal: %s — %s", jobID, apiErr.Code, apiErr.Message)
	default:
		logger.Errorf(apiErr.Underlying, "job %s terminal: %s — %s", jobID, apiErr.Code, apiErr.Message)
	}
	if w.stats != nil {
		w.stats.Inc(apiErr.Code)
	}
}
```

import 块补 `"github.com/nicoxiang/geektime-downloader/internal/pkg/logger"`。

`runJob` 错误块改为（在 `apiErr := apperr.MapError(err)` 之后、原 switch 之前插入 `recordTerminal` + `touchActive`）：
```go
	course, folder, err := w.exec.ExecuteDownload(jobCtx, job.Request, reporter)
	w.touchActive()
	if err != nil {
		apiErr := apperr.MapError(err)
		w.recordTerminal(jobID, apiErr)
		if errors.Is(err, context.DeadlineExceeded) {
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusFailed, "watchdog_timeout", apiErr)
			return
		}
		switch apiErr.Code {
		case apperr.CodeAuthExpired:
			w.pausedAuth.Store(true)
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusWaitingAuth, apiErr.Message, apiErr)
		case apperr.CodeRateLimited:
			w.applyRateLimitCooldown()
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusWaitingRateLimit, apiErr.Message, apiErr)
		case apperr.CodeCancelled:
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusCancelled, apiErr.Message, apiErr)
		default:
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusFailed, apiErr.Message, apiErr)
		}
		return
	}
```

> 实现者：原 `runJob` 中 `if errors.Is(err, context.DeadlineExceeded)` 在 `apiErr := MapError(err)` **之前**；新版改为先 `apiErr := MapError(err)` + `recordTerminal`，再判 `DeadlineExceeded`。保留硬超时→`StatusFailed`+`watchdog_timeout` 语义。`recordTerminal` 对 `CodeTimeout`（即 DeadlineExceeded 映射码）记 Warn + Inc TIMEOUT。

`stabilityLoop` 每 tick 顶部补 `w.touchActive()`：
```go
func (w *Worker) stabilityLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.touchActive()
			// ... existing MarkStaleJobs + cooldown-resume logic ...
		}
	}
}
```

- [ ] **Step 4: Update worker_test.go NewWorker call sites**

`internal/job/worker_test.go` 现有 `NewWorker(store, &fakeExecutor{...}, nil, Stability{...})` 调用（T8/T9 测试中）补第 5 参 `nil`：
```go
NewWorker(store, &fakeExecutor{err: context.DeadlineExceeded}, nil, Stability{...}, nil)
NewWorker(store, &fakeExecutor{}, nil, Stability{...}, nil)
```

- [ ] **Step 5: Update serve.go**

`cmd/serve.go`：
```go
		stats := &job.Stats{}
		worker := job.NewWorker(store, dlSvc, authMgr.GetClient, job.Stability{
			JobTimeout:        cfg.JobTimeout,
			HeartbeatTimeout:  cfg.HeartbeatTimeout,
			RateLimitCooldown: cfg.RateLimitCooldown,
		}, stats)
```

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/job/ -v && go build ./...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/job/worker.go internal/job/worker_test.go cmd/serve.go
git commit -m "feat(job): worker holds Stats, leveled terminal logging, exposure methods"
```

---

### Task 6: api — /health 与 /downloads/{id} 新字段

**Files:**
- Modify: `internal/job/model.go`（`Job.RuntimeSeconds`）
- Modify: `internal/api/handlers.go`（`handleHealth`/`handleGetDownload`）
- Modify: `internal/api/response.go`（`isoTime`/`isoTimePtr` helpers）
- Test: `internal/api/handlers_test.go`（新建）

**Interfaces:**
- Produces: `handleHealth` 输出含 `uptime_seconds`/`last_active_at`/`stale_jobs`/`error_counts`/`rate_limit_cooldown_until`；`handleGetDownload` 给 running job 填 `runtime_seconds`。
- Consumes: T5 `worker.Stats/LastActiveAt/Uptime/RateLimitUntil`、T2 `store.CountStaleJobs`。

- [ ] **Step 1: Write failing test**

新建 `internal/api/handlers_test.go`（包 `api_test`）：

```go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/api"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/job"
	"github.com/nicoxiang/geektime-downloader/internal/progress"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

type fakeExec struct{}

func (fakeExec) SetClient(*geektime.Client)                              {}
func (fakeExec) Client() *geektime.Client                                { return &geektime.Client{} }
func (fakeExec) ExecuteDownload(context.Context, service.DownloadRequest, progress.Reporter) (geektime.Course, string, error) {
	return geektime.Course{}, "", nil
}

func newTestServer(t *testing.T) (*api.Server, *job.Store, *job.Stats) {
	t.Helper()
	store, err := job.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stats := &job.Stats{}
	w := job.NewWorker(store, fakeExec{}, nil, job.Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: time.Minute,
	}, stats)
	srv := api.NewServer(nil, store, w, nil, "test", "k", nil)
	return srv, store, stats
}

func TestHandleHealthNewFields(t *testing.T) {
	srv, store, stats := newTestServer(t)
	stats.Inc("AUTH_EXPIRED")
	_ = store // (no job needed)
	_ = store.CountStaleJobs(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.SetBasicAuth("", "k") // not used; health is unprotected
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"uptime_seconds", "last_active_at", "stale_jobs", "error_counts", "rate_limit_cooldown_until"} {
		if _, ok := env.Data[k]; !ok {
			t.Fatalf("health missing field %s", k)
		}
	}
}

func TestHandleGetDownloadRuntimeSeconds(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// mark running with started_at 10s ago
	past := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	if _, err := store.DB().ExecContext(ctx, `UPDATE jobs SET status='running', started_at=?, updated_at=? WHERE id=?`, past, past, j.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+j.ID, nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var env struct {
		Data struct {
			RuntimeSeconds int64 `json:"runtime_seconds"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Data.RuntimeSeconds <= 0 {
		t.Fatalf("running job runtime_seconds should be >0, got %d", env.Data.RuntimeSeconds)
	}
}
```

> 实现者：`api.NewServer` 签名为 `(authMgr, store, worker, svc, version, apiKey, onCookies)`；authMgr/svc 可传 nil（health/get 路径不用）。health 路由在 `mux` 中未加 `protect`（无鉴权），故直接命中。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -v`
Expected: FAIL（新字段未输出；`runtime_seconds` 未填）

- [ ] **Step 3: Implement model.go**

`internal/job/model.go` 的 `Job` struct 加：
```go
	RuntimeSeconds  int64                 `json:"runtime_seconds,omitempty"`
```

- [ ] **Step 4: Implement response.go helpers**

`internal/api/response.go` 末尾加：
```go
import "time"

// isoTimePtr returns an RFC3339 string for non-zero t, or JSON null for zero.
func isoTimePtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
```

- [ ] **Step 5: Implement handlers.go**

`handleHealth`：
```go
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	workerStatus := "idle"
	activeJob := s.worker.ActiveJobID()
	if activeJob != "" {
		workerStatus = "busy"
	}
	staleCount, _ := s.store.CountStaleJobs(r.Context())
	writeOK(w, map[string]any{
		"status":                     "ok",
		"version":                    s.version,
		"chrome_available":           chromeAvailable(),
		"worker_status":              workerStatus,
		"active_job_id":              nilString(activeJob),
		"uptime_seconds":             int64(s.worker.Uptime().Seconds()),
		"last_active_at":             isoTimePtr(s.worker.LastActiveAt()),
		"stale_jobs":                 staleCount,
		"error_counts":               s.worker.Stats(),
		"rate_limit_cooldown_until":  isoTimePtr(s.worker.RateLimitUntil()),
	})
}
```

`handleGetDownload`：
```go
func (s *Server) handleGetDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if j.Status == StatusRunning && j.StartedAt != nil {
		j.RuntimeSeconds = int64(time.Since(*j.StartedAt).Seconds())
	}
	writeOK(w, j)
}
```

> 实现者：`StatusRunning` 常量在 `job` 包；`api` 包已 import `job`。`time` import 加到 handlers.go。

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/api/ -v && go build ./...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/job/model.go internal/api/handlers.go internal/api/response.go internal/api/handlers_test.go
git commit -m "feat(api): expose health observability fields and job runtime_seconds"
```

---

## Self-Review（计划作者自检）

**1. Spec 覆盖：**

| Spec 项 | 任务 |
|---------|------|
| §3.1 job.Stats | T1 |
| §3.2 worker 持有 stats/lastActiveAt/startedAt、分级日志、暴露方法 | T5 |
| §3.3 store.CountStaleJobs | T2 |
| §3.4 handleHealth/handleGetDownload 新字段、Job.RuntimeSeconds | T6 |
| §3.5 tsparser ctx | T3 |
| §3.6 mergeTSFiles ctx | T4 |
| §3.7 serve 注入 stats | T5（serve.go 改动并入 T5） |

无遗漏。

**2. 占位符扫描：** 无 TBD/TODO；每步含可执行代码或命令。T3 的 tsparser 测试给了主方案 + 退化方案（直接构造 tsStream），保证可落地。

**3. 类型一致性：**
- `NewWorker(store, exec, clientProvider, st Stability, stats *Stats)` 在 T5 定义；T5 worker_test 与 serve 调用一致；T6 `newTestServer` 用同签名（传 `fakeExec{}` 满足 `executor`，第 5 参 `&job.Stats{}`）。
- `m3u8.NewTSParser(ctx, data, key)` / `Decrypt(ctx)` 在 T3 定义；T4 video 调用一致。
- `mergeTSFiles(ctx, ...)` 在 T4 定义；`download` 调用一致。
- `Stats.Inc/Snapshot` 在 T1 定义；T5 `recordTerminal` 调用一致；T6 测试调用一致。
- `worker.Stats()/LastActiveAt()/Uptime()/RateLimitUntil()` 在 T5 定义；T6 handlers 调用一致。
- `store.CountStaleJobs` 在 T2 定义；T6 handleHealth 调用一致。

**4. 中间态说明：** T3 改 tsparser 签名后 `go build ./...` 失败（video 调用点未改），T4 立即修复——T3 Step 4 只跑 `go test ./internal/m3u8/`，不强求全量 build。T4 恢复全量绿。

---

## Execution Handoff

计划已保存到 `docs/superpowers/plans/2026-07-16-observability-ctx.md`。两种执行方式：

**1. Subagent-Driven（推荐）** — 每个 Task 派一个全新 subagent 实现，任务间两阶段评审，迭代快。

**2. Inline Execution** — 在当前会话用 executing-plans 批量执行，带检查点评审。

选哪种？
