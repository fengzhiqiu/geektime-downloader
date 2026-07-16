# 下载稳定性改造 (P0+P1) 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 硬化 serve 模式下载链路——文件下载加超时+状态码检查避免静默坏文件与卡死；Worker 加看门狗（硬超时+心跳）；限流加全局冷却+自动恢复；视频下载上报细粒度进度刷新心跳。

**Architecture:** 改动集中在 `pkg/downloader`（超时+状态码+`StatusError`）、`geektime/client`（抽 `CheckStatus`）、`video`（透传 reporter+segmentTimeout、修吞错、getPlayInfo 检查）、`job/worker`+`store`（看门狗 ticker、冷却闸门、`MarkStaleJobs`/`ResumeRateLimitJobs`）、`progress`/`job/model`（`OnArticleProgress`+`Done/Total`）、`config`+`cmd/serve`（4 个新 flag）。不改 DB schema，复用 `updated_at` 做心跳。

**Tech Stack:** Go 1.22+（ServeMux 路由）、`net/http`+`httptest`、`go-resty/resty/v2`、`modernc.org/sqlite`、标准库 `testing`。

## Global Constraints

- 阈值默认值（精确）：`JobTimeout=60m`、`HeartbeatTimeout=10m`、`RateLimitCooldown=120s`（指数 `×1→×2→×4` 封顶 30min）、`SegmentTimeout=60s`。
- 不改 SQLite schema、不做 migration；`CurrentArticle.Done/Total` 作为 `progress_json` 字段向前兼容。
- CLI FSM 模式（`reporter==nil`）行为必须不变；所有 reporter 调用需 nil-guard。
- P2（health 字段增强、错误码计数日志、tsparser/merge 响应 ctx）**不在本计划**。
- Go module：`github.com/nicoxiang/geektime-downloader`。测试命令：`go test ./internal/...`。构建：`go build ./...`。
- 每个任务结束 `go build ./...` + 相关包 `go test` 通过后提交；提交信息沿用仓库 conventional 风格（`feat:`/`fix:`/`refactor:`）。
- 当前已在分支 `feat/download-stability`，所有任务在此分支提交。

---

## File Structure

| 文件 | 责任 | 任务 |
|------|------|------|
| `internal/apperr/errors.go` | `MapError` 增 `DeadlineExceeded`→TIMEOUT | T1 |
| `internal/config/config.go` | `AppConfig` 增 4 个 `time.Duration` 字段 | T2 |
| `internal/config/validator.go` | `ValidateServeConfig` 增范围校验 | T2 |
| `internal/progress/reporter.go` | `Reporter` 增 `OnArticleProgress`；`Nop` 实现 | T3 |
| `internal/job/model.go` | `CurrentArticle` 增 `Done/Total` | T3 |
| `internal/pkg/downloader/downloader.go` | `StatusError`+`httpClient`+状态码检查+`segmentTimeout`参数+retry 区分 | T4 |
| `internal/geektime/client.go` | 导出 `CheckStatus`，`do()` 复用 | T5 |
| `internal/video/video.go` | getPlayInfo 检查；`DownloadMP4` 返回 err；透传 reporter+segmentTimeout；per-seg 进度；`StatusError` 翻译 | T6 |
| `internal/course/downloader.go` | jitter 加大；调用 video 时传 reporter | T7 |
| `internal/job/store.go` | `MarkStaleJobs`/`ResumeRateLimitJobs` | T8 |
| `internal/job/worker.go` | 硬超时；心跳 ticker；限流冷却闸门；自动恢复 ticker | T9, T10 |
| `cmd/serve.go` | 4 个 flag + 透传 config 到 worker/downloader | T11 |

---

### Task 1: apperr — DeadlineExceeded → TIMEOUT

**Files:**
- Modify: `internal/apperr/errors.go`（`MapError` 的 default 分支前插入）
- Test: `internal/apperr/errors_test.go`

**Interfaces:**
- Produces: `MapError(context.DeadlineExceeded)` → `&APIError{Code:CodeTimeout, Action:"RETRY", Retryable:true, HTTPStatus:504}`

- [ ] **Step 1: Write failing test**

追加到 `internal/apperr/errors_test.go`：

```go
func TestMapErrorDeadlineExceeded(t *testing.T) {
	err := context.DeadlineExceeded
	apiErr := MapError(err)
	if apiErr.Code != CodeTimeout {
		t.Fatalf("want CodeTimeout, got %s", apiErr.Code)
	}
	if !apiErr.Retryable {
		t.Fatal("DeadlineExceeded should be retryable")
	}
	if apiErr.HTTPStatus != 504 {
		t.Fatalf("want 504, got %d", apiErr.HTTPStatus)
	}
}
```

若文件无 `context` import，加 `"context"`。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apperr/ -run TestMapErrorDeadlineExceeded -v`
Expected: FAIL（`DeadlineExceeded` 落入 default → CodeInternal，非 CodeTimeout）

- [ ] **Step 3: Implement**

在 `internal/apperr/errors.go` 的 `MapError` switch 中，`errors.Is(err, context.Canceled)` case 之后、`default` 之前插入：

```go
case errors.Is(err, context.DeadlineExceeded):
	return &APIError{
		Code: CodeTimeout, Message: "任务执行超时（看门狗触发）",
		Action: "RETRY", Retryable: true, HTTPStatus: 504, Underlying: err,
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apperr/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/apperr/errors.go internal/apperr/errors_test.go
git commit -m "feat(apperr): map context.DeadlineExceeded to TIMEOUT"
```

---

### Task 2: config — 新增 4 个 Duration 字段 + 校验

**Files:**
- Modify: `internal/config/config.go`（`AppConfig`）
- Modify: `internal/config/validator.go`（`ValidateServeConfig`）
- Test: `internal/config/validator_test.go`（新建）

**Interfaces:**
- Produces: `AppConfig.JobTimeout/HeartbeatTimeout/RateLimitCooldown/SegmentTimeout`（`time.Duration`）

- [ ] **Step 1: Write failing test**

新建 `internal/config/validator_test.go`：

```go
package config

import (
	"testing"
	"time"
)

func validServeCfg() AppConfig {
	return AppConfig{
		Quality: "sd", DownloadComments: 1, ColumnOutputType: 7,
		LogLevel: "info", Interval: 1,
		JobTimeout: 60 * time.Minute, HeartbeatTimeout: 10 * time.Minute,
		RateLimitCooldown: 120 * time.Second, SegmentTimeout: 60 * time.Second,
	}
}

func TestValidateServeConfigStabilityZeroRejected(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AppConfig)
	}{
		{"JobTimeout zero", func(c *AppConfig) { c.JobTimeout = 0 }},
		{"HeartbeatTimeout zero", func(c *AppConfig) { c.HeartbeatTimeout = 0 }},
		{"RateLimitCooldown zero", func(c *AppConfig) { c.RateLimitCooldown = 0 }},
		{"SegmentTimeout zero", func(c *AppConfig) { c.SegmentTimeout = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validServeCfg()
			tc.mut(&c)
			if err := ValidateServeConfig(&c); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestValidateServeConfigStabilityZeroRejected -v`
Expected: FAIL（字段不存在，编译错误）

- [ ] **Step 3: Add fields to AppConfig**

`internal/config/config.go` 的 `AppConfig` struct 末尾追加：

```go
	JobTimeout          time.Duration
	HeartbeatTimeout    time.Duration
	RateLimitCooldown   time.Duration
	SegmentTimeout       time.Duration
```

- [ ] **Step 4: Add validation**

`internal/config/validator.go` 的 `ValidateServeConfig` 内，`validateTiming` 调用后追加：

```go
	if err := validateStability(cfg); err != nil {
		return err
	}
```

并在文件末尾追加：

```go
func validateStability(cfg *AppConfig) error {
	if cfg.JobTimeout <= 0 {
		return fmt.Errorf("argument 'job-timeout' must be greater than 0")
	}
	if cfg.HeartbeatTimeout <= 0 {
		return fmt.Errorf("argument 'heartbeat-timeout' must be greater than 0")
	}
	if cfg.RateLimitCooldown <= 0 {
		return fmt.Errorf("argument 'rate-limit-cooldown' must be greater than 0")
	}
	if cfg.SegmentTimeout <= 0 {
		return fmt.Errorf("argument 'segment-timeout' must be greater than 0")
	}
	return nil
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/validator.go internal/config/validator_test.go
git commit -m "feat(config): add job/heartbeat/ratelimit/segment timeout fields"
```

---

### Task 3: progress + model — OnArticleProgress + Done/Total

**Files:**
- Modify: `internal/progress/reporter.go`
- Modify: `internal/job/model.go`
- Test: `internal/progress/reporter_test.go`（新建）

**Interfaces:**
- Produces: `progress.Reporter.OnArticleProgress(aid, done, total int)`；`progress.Nop.OnArticleProgress`；`job.CurrentArticle{AID,Title,Phase,Done,Total}`

- [ ] **Step 1: Write failing test**

新建 `internal/progress/reporter_test.go`：

```go
package progress

type captureReporter struct {
	progresses []progressEvent
}
type progressEvent struct{ aid, done, total int }

func (c *captureReporter) OnArticleStart(int, string, string)      {}
func (c *captureReporter) OnArticleComplete(int, []string)        {}
func (c *captureReporter) OnArticleSkipped(int, string)           {}
func (c *captureReporter) OnArticleFailed(int, error)            {}
func (c *captureReporter) OnArticleProgress(aid, done, total int) {
	c.progresses = append(c.progresses, progressEvent{aid, done, total})
}

func TestNopOnArticleProgress(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Nop.OnArticleProgress must not panic: %v", r)
		}
	}()
	var n Nop
	n.OnArticleProgress(1, 2, 10)
}
```

> 注：`captureReporter` 用于后续 video 任务验证 per-segment 回调；此处先保证接口可编译。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/progress/ -run TestNopOnArticleProgress -v`
Expected: FAIL（`OnArticleProgress` 未定义，编译错误）

- [ ] **Step 3: Add method to Reporter + Nop**

`internal/progress/reporter.go`：

```go
// Reporter receives download progress events. All methods are optional no-ops when nil.
type Reporter interface {
	OnArticleStart(aid int, title, phase string)
	OnArticleComplete(aid int, files []string)
	OnArticleSkipped(aid int, reason string)
	OnArticleFailed(aid int, err error)
	OnArticleProgress(aid, done, total int)
}

// Nop ignores all progress events.
type Nop struct{}

func (Nop) OnArticleStart(int, string, string)      {}
func (Nop) OnArticleComplete(int, []string)         {}
func (Nop) OnArticleSkipped(int, string)             {}
func (Nop) OnArticleFailed(int, error)               {}
func (Nop) OnArticleProgress(int, int, int)          {}
```

- [ ] **Step 4: Add Done/Total to CurrentArticle**

`internal/job/model.go` 的 `CurrentArticle` struct 加字段（保持现有字段）：

现有（`internal/job/model.go:50-54`）：

```go
type CurrentArticle struct {
	AID   int    `json:"aid"`
	Title string `json:"title"`
	Phase string `json:"phase"`
}
```

改为（仅追加 `Done/Total` 两行，不动已有字段与 tag）：

```go
type CurrentArticle struct {
	AID   int    `json:"aid"`
	Title string `json:"title"`
	Phase string `json:"phase"`
	Done  int    `json:"done,omitempty"`
	Total int    `json:"total,omitempty"`
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/progress/ ./internal/job/ -v && go build ./...`
Expected: PASS；`job` 包现有测试不破。

- [ ] **Step 6: Commit**

```bash
git add internal/progress/reporter.go internal/progress/reporter_test.go internal/job/model.go
git commit -m "feat(progress): add OnArticleProgress and CurrentArticle.Done/Total"
```

---

### Task 4: pkg/downloader — 超时 + 状态码检查 + StatusError + retry 区分

**Files:**
- Modify: `internal/pkg/downloader/downloader.go`
- Modify: `internal/video/video.go:213` 和 `:258`（两处 `DownloadFileConcurrently` 调用补 `0` 占位参数，保持编译）
- Test: `internal/pkg/downloader/downloader_test.go`（新建）

**Interfaces:**
- Produces: `downloader.StatusError{StatusCode int; Body string}`（导出，供 video 翻译）；`DownloadFileConcurrently(ctx, filepath, url, headers, concurrency, segmentTimeout time.Duration)` 新增末参 `segmentTimeout`（<=0 时内部用 60s 默认）。

- [ ] **Step 1: Write failing tests**

新建 `internal/pkg/downloader/downloader_test.go`：

```go
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadFileConcurrentlyStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.WriteHeader(452)
			return
		}
		w.WriteHeader(452)
		_, _ = io.WriteString(w, "auth expired body")
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "out.bin")
	_, err := DownloadFileConcurrently(context.Background(), dst, srv.URL, nil, 2, time.Second)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 452 {
		t.Fatalf("want StatusError{452}, got %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("坏文件不应落盘")
	}
}

func TestDownloadFileConcurrentlyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			return
		}
		time.Sleep(2 * time.Second) // 超过 segmentTimeout
		w.WriteHeader(200)
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "out.bin")
	ctx := context.Background()
	_, err := DownloadFileConcurrently(ctx, dst, srv.URL, nil, 1, 100*time.Millisecond)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
}

func TestRetryDoesNotRetryStatusError(t *testing.T) {
	calls := 0
	err := retry(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return &StatusError{StatusCode: 403, Body: "forbidden"}
	})
	if calls != 1 {
		t.Fatalf("StatusError must not retry, calls=%d", calls)
	}
	if err == nil {
		t.Fatal("want error")
	}
}

func TestRetryRetriesNetworkError(t *testing.T) {
	calls := 0
	_ = retry(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return fmt.Errorf("transient")
	})
	if calls != 3 {
		t.Fatalf("network error should retry up to 3, calls=%d", calls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pkg/downloader/ -v`
Expected: FAIL（`StatusError` 未定义、签名不匹配，编译错误）

- [ ] **Step 3: Implement downloader.go**

在 `internal/pkg/downloader/downloader.go` 顶部 import 加 `"math/rand"`，并在 `Part` 结构定义后追加：

```go
// StatusError is returned when a file download response is not 2xx,
// so callers never mistake an error page for file content.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("download responded status %d: %s", e.StatusCode, e.Body)
}

const defaultSegmentTimeout = 60 * time.Second

var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:      90 * time.Second,
	},
}

func segmentTimeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultSegmentTimeout
	}
	return d
}
```

改 `DownloadFileConcurrently` 签名加 `segmentTimeout time.Duration`，HEAD 部分改为：

```go
func DownloadFileConcurrently(ctx context.Context, filepath string, url string, headers map[string]string, concurrency int, segmentTimeout time.Duration) (int64, error) {
	headCtx, headCancel := context.WithTimeout(ctx, segmentTimeoutOrDefault(segmentTimeout))
	req, err := http.NewRequestWithContext(headCtx, "HEAD", url, nil)
	if err != nil {
		headCancel()
		return 0, err
	}
	for k, v := range headers {
		req.Header.Add(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		headCancel()
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		headCancel()
		return 0, &StatusError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	_ = resp.Body.Close()
	headCancel()

	fileSize, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if fileSize <= 0 {
		// ... existing empty-file branch unchanged ...
	}
```

> 实现者：保留现有 `fileSize <= 0` 分支与后续并发下载主体不变；仅在 `download` 调用处透传 `segmentTimeout`。

`download` 函数签名加 `segmentTimeout time.Duration` 与 `headers map[string]string`（注意：当前 `download` 未接收 headers，需补上以便 GET 带头；调用处 `download(ctx, concurrency, i, chunkSize, url, results)` 改为 `download(ctx, concurrency, i, chunkSize, url, headers, segmentTimeout, results)`）。`download` 内：

```go
func download(ctx context.Context, workers int, index int, chunkSize int64, url string, headers map[string]string, segmentTimeout time.Duration, c chan Part) error {
	start := int64(index) * chunkSize
	dataRange := fmt.Sprintf("bytes=%d-%d", start, start+chunkSize-1)
	if index == workers-1 {
		dataRange = fmt.Sprintf("bytes=%d-", start)
	}
	timeout := segmentTimeoutOrDefault(segmentTimeout)
	err := retry(ctx, 3, 700*time.Millisecond, func() error {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, "GET", url, nil)
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Add(k, v)
		}
		req.Header.Add("Range", dataRange)
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return &StatusError{StatusCode: resp.StatusCode, Body: string(body)}
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		c <- Part{Index: index, Offset: start, Data: body}
		return nil
	})
	return err
}
```

改 `retry` 区分错误类型：

```go
func retry(ctx context.Context, attempts int, sleep time.Duration, f func() error) (err error) {
	for i := 0; i < attempts; i++ {
		if i > 0 {
			jitter := time.Duration(rand.Intn(int(sleep/5) + 1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sleep + jitter):
			}
			sleep = sleep * 2
			logger.Infof("retry happen, times: %s", strconv.Itoa(i))
		}
		err = f()
		// context.Canceled = parent (job) cancelled → terminal, do not retry.
		// Per-request DeadlineExceeded (segment timeout) IS retried (transient);
		// the loop's ctx.Done() guard above prevents retrying when the parent
		// job ctx itself has expired/cancelled.
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		var se *StatusError
		if errors.As(err, &se) {
			return err // server-side rejection (4xx): do not retry
		}
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}
```

- [ ] **Step 4: Update video call sites (pass 0 placeholder)**

`internal/video/video.go:213` 与 `:258` 的 `downloader.DownloadFileConcurrently(ctx, dst, u, headers, N)` 调用各补末参 `0`：

```go
_, err := downloader.DownloadFileConcurrently(ctx, dst, mp4URL, headers, 5, 0)
```
```go
fileSize, err := downloader.DownloadFileConcurrently(ctx, dst, u, headers, concurrency, 0)
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/pkg/downloader/ -v && go build ./...`
Expected: PASS；全量构建通过。

- [ ] **Step 6: Commit**

```bash
git add internal/pkg/downloader/downloader.go internal/pkg/downloader/downloader_test.go internal/video/video.go
git commit -m "feat(downloader): add timeout, status-code checks, StatusError, retry distinction"
```

---

### Task 5: geektime/client — 导出 CheckStatus，do() 复用

**Files:**
- Modify: `internal/geektime/client.go`
- Test: `internal/geektime/client_test.go`（新建）

**Interfaces:**
- Produces: `geektime.CheckStatus(resp *resty.Response) error`（200→nil；451→`ErrGeekTimeRateLimit`；452→`ErrAuthFailed`；其他非 2xx→`ErrGeekTimeAPIBadCode`）

- [ ] **Step 1: Write failing test**

新建 `internal/geektime/client_test.go`：

```go
package geektime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestCheckStatus(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{200, nil},
		{204, nil},
		{451, ErrGeekTimeRateLimit},
		{452, ErrAuthFailed},
		{500, ErrGeekTimeAPIBadCode{}},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			resp, err := resty.New().R().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckStatus(resp)
			if tc.want == nil && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/geektime/ -run TestCheckStatus -v`
Expected: FAIL（`CheckStatus` 未定义）

- [ ] **Step 3: Implement + refactor do()**

在 `internal/geektime/client.go` 的 `do` 函数前追加：

```go
// CheckStatus maps a non-2xx response to a sentinel error. Returns nil for 2xx.
// Shared by do() (geektime APIs) and video.getPlayInfo (VOD URL).
func CheckStatus(resp *resty.Response) error {
	statusCode := resp.RawResponse.StatusCode
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}
	logNotOkResponse(resp)
	switch statusCode {
	case 451:
		return ErrGeekTimeRateLimit
	case 452:
		return ErrAuthFailed
	default:
		return ErrGeekTimeAPIBadCode{
			Path:           resp.RawResponse.Request.URL.String(),
			ResponseString: resp.String(),
		}
	}
}
```

把 `do()` 中的：

```go
	if statusCode != 200 {
		logNotOkResponse(resp)
		switch statusCode {
		case 451:
			return nil, ErrGeekTimeRateLimit
		case 452:
			return nil, ErrAuthFailed
		}
	}
```

替换为：

```go
	if err := CheckStatus(resp); err != nil {
		return nil, err
	}
```

（`statusCode` 局部变量若不再被引用，删掉其声明与对应 `resp.RawResponse.StatusCode` 赋值，避免未使用编译错误。）

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/geektime/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/geektime/client.go internal/geektime/client_test.go
git commit -m "refactor(geektime): export CheckStatus and reuse in do()"
```

---

### Task 6: video — getPlayInfo 检查 + 修 DownloadMP4 吞错 + StatusError 翻译

**Files:**
- Modify: `internal/video/video.go`
- Test: `internal/video/video_test.go`（新建）

**Interfaces:**
- Produces: video 内 `translateDownloadErr(err) error`（451/452 StatusError → geektime 语义错误）；`getPlayInfo` 非 200 返回映射错误；`DownloadMP4` 失败冒泡。

- [ ] **Step 1: Write failing tests**

新建 `internal/video/video_test.go`：

```go
package video

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/pkg/downloader"
)

func TestTranslateDownloadErr(t *testing.T) {
	if err := translateDownloadErr(&downloader.StatusError{StatusCode: 452}); !errors.Is(err, geektime.ErrAuthFailed) {
		t.Fatalf("452 want ErrAuthFailed, got %v", err)
	}
	if err := translateDownloadErr(&downloader.StatusError{StatusCode: 451}); !errors.Is(err, geektime.ErrGeekTimeRateLimit) {
		t.Fatalf("451 want ErrGeekTimeRateLimit, got %v", err)
	}
	if err := translateDownloadErr(&downloader.StatusError{StatusCode: 403}); err == nil {
		t.Fatal("403 should surface as non-nil error")
	}
}

func TestDownloadMP4ReturnsErrorOn403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	err := DownloadMP4(context.Background(), "t", dir, []string{srv.URL}, false)
	if err == nil {
		t.Fatal("want error on 403, got nil")
	}
}

func TestGetPlayInfoNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	c := geektime.NewClient(nil)
	_, err := getPlayInfo(c, srv.URL, "sd")
	if err == nil {
		t.Fatal("want error on non-200 getPlayInfo")
	}
	_ = os.Stat(filepath.Join(t.TempDir(), "x")) // keep imports used
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/video/ -v`
Expected: FAIL（`translateDownloadErr` 未定义；`getPlayInfo` 对 403 返回 nil；`DownloadMP4` 返回 nil）

- [ ] **Step 3: Add translateDownloadErr helper**

在 `internal/video/video.go` 末尾追加：

```go
// translateDownloadErr maps a downloader.StatusError (from CDN/VOD responses)
// to geektime sentinel errors so the job state machine can react to auth/rate-limit.
func translateDownloadErr(err error) error {
	if err == nil {
		return nil
	}
	var se *downloader.StatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case 451:
			return geektime.ErrGeekTimeRateLimit
		case 452:
			return geektime.ErrAuthFailed
		}
	}
	return err
}
```

import 块补 `"errors"` 与 `"github.com/nicoxiang/geektime-downloader/internal/pkg/downloader"`。

- [ ] **Step 4: getPlayInfo uses CheckStatus**

把 `getPlayInfo` 的：

```go
	_, err := client.RestyClient.R().
		SetResult(&getPlayInfoResp).
		Get(playInfoURL)
	if err != nil {
		return playInfo, err
	}
```

改为：

```go
	resp, err := client.RestyClient.R().
		SetResult(&getPlayInfoResp).
		Get(playInfoURL)
	if err != nil {
		return playInfo, err
	}
	if err := geektime.CheckStatus(resp); err != nil {
		return playInfo, err
	}
```

- [ ] **Step 5: Fix DownloadMP4 swallowed error**

`DownloadMP4` 内：

```go
		if err != nil {
			logger.Errorf(err, "Failed to download single article mp4 video, title: %s, mp4URL: %s", title, mp4URL)
			return nil
		}
```

改为：

```go
		if err != nil {
			logger.Errorf(err, "Failed to download single article mp4 video, title: %s, mp4URL: %s", title, mp4URL)
			return translateDownloadErr(err)
		}
```

并在 `download` 的 ts 循环里把 `DownloadFileConcurrently` 返回值包一层翻译：

```go
		fileSize, err := downloader.DownloadFileConcurrently(ctx, dst, u, headers, concurrency, 0)
		if err != nil {
			return translateDownloadErr(err)
		}
```

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/video/ -v && go build ./...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/video/video.go internal/video/video_test.go
git commit -m "fix(video): check getPlayInfo status, stop swallowing DownloadMP4 errors, translate StatusError"
```

---

### Task 7: job/store — MarkStaleJobs + ResumeRateLimitJobs

**Files:**
- Modify: `internal/job/store.go`
- Test: `internal/job/store_test.go`（追加）

**Interfaces:**
- Produces: `(*Store).MarkStaleJobs(ctx, heartbeatTimeout time.Duration) error`；`(*Store).ResumeRateLimitJobs(ctx) (int64, error)`。

- [ ] **Step 1: Write failing tests**

追加到 `internal/job/store_test.go`（实现者先 Read 该文件，复用其已有的 `openTestStore` / 建库辅助；若无则用 `OpenStore(filepath.Join(t.TempDir(),"t.db"))`）：

```go
func TestMarkStaleJobs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil { t.Fatal(err) }
	// 标记 running，updated_at 设为 1 小时前
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='running', updated_at=? WHERE id=?`, past, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkStaleJobs(ctx, 10*time.Minute); err != nil { t.Fatal(err) }
	got, _ := store.GetJob(ctx, j.ID)
	if got.StatusReason != "stale_progress" {
		t.Fatalf("want status_reason=stale_progress, got %q", got.StatusReason)
	}
	if got.Status != "running" {
		t.Fatalf("status must stay running, got %q", got.Status)
	}
}

func TestResumeRateLimitJobs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 2, Mode: "all"})
	if err != nil { t.Fatal(err) }
	_ = store.UpdateJobStatus(ctx, j.ID, StatusWaitingRateLimit, "rate limited", nil)
	n, err := store.ResumeRateLimitJobs(ctx)
	if err != nil { t.Fatal(err) }
	if n != 1 { t.Fatalf("want 1 resumed, got %d", n) }
	got, _ := store.GetJob(ctx, j.ID)
	if got.Status != StatusPending {
		t.Fatalf("want pending, got %q", got.Status)
	}
}
```

> 实现者：若 `newTestStore` 不存在，用 `func newTestStore(t *testing.T) *Store { s, err := OpenStore(filepath.Join(t.TempDir(), "t.db")); if err != nil { t.Fatal(err) }; return s }` 并确保 import 齐全。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/job/ -run 'TestMarkStaleJobs|TestResumeRateLimitJobs' -v`
Expected: FAIL（方法未定义）

- [ ] **Step 3: Implement**

在 `internal/job/store.go` 的 `ResumeWaitingAuthJobs` 后追加：

```go
// MarkStaleJobs flags running jobs whose progress has not updated within
// heartbeatTimeout as stale_progress (status stays running). Does not force-end.
func (s *Store) MarkStaleJobs(ctx context.Context, heartbeatTimeout time.Duration) error {
	cutoff := time.Now().UTC().Add(-heartbeatTimeout).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status_reason = ?, updated_at = ?
		WHERE status = ? AND updated_at < ? AND status_reason = ''
	`, "stale_progress", now, StatusRunning, cutoff)
	return err
}

// ResumeRateLimitJobs moves waiting_rate_limit jobs back to pending after cooldown.
func (s *Store) ResumeRateLimitJobs(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, status_reason = '', error_json = '', updated_at = ?
		WHERE status = ?
	`, StatusPending, now, StatusWaitingRateLimit)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
```

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/job/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/job/store.go internal/job/store_test.go
git commit -m "feat(job): add MarkStaleJobs and ResumeRateLimitJobs store methods"
```

---

### Task 8: job/worker — 硬超时看门狗 + executor 接口

**Files:**
- Modify: `internal/job/worker.go`
- Modify: `cmd/serve.go`（NewWorker 调用处）
- Test: `internal/job/worker_test.go`（新建）

**Interfaces:**
- Produces: `job.Stability{JobTimeout, HeartbeatTimeout, RateLimitCooldown time.Duration}`；`job.NewWorker(store, exec, clientProvider, st Stability)`，其中 `exec` 为新接口 `job.executor`（`*service.DownloadService` 自动满足）；`runJob` 用 `context.WithTimeout(parent, JobTimeout)`，`DeadlineExceeded` → `StatusFailed`+reason `watchdog_timeout`。

> 说明：引入 `executor` 接口仅为可测试性（注入返回 `DeadlineExceeded` 的假实现），不改变生产路径行为。

- [ ] **Step 1: Write failing test**

新建 `internal/job/worker_test.go`：

```go
package job

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/progress"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

type fakeExecutor struct {
	err error
	c  *geektime.Client
}

func (f *fakeExecutor) SetClient(*geektime.Client)                              {}
func (f *fakeExecutor) Client() *geektime.Client {
	if f.c != nil {
		return f.c
	}
	return &geektime.Client{} // non-nil so runJob proceeds past the session check
}
func (f *fakeExecutor) ExecuteDownload(ctx context.Context, req service.DownloadRequest, rep progress.Reporter) (geektime.Course, string, error) {
	return geektime.Course{}, "", f.err
}

func newTestStoreTB(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil { t.Fatal(err) }
	return s
}

func TestRunJobWatchdogTimeout(t *testing.T) {
	store := newTestStoreTB(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil { t.Fatal(err) }

	w := NewWorker(store, &fakeExecutor{err: context.DeadlineExceeded}, nil, Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: time.Minute,
	})
	w.runJob(ctx, j.ID)

	got, _ := store.GetJob(ctx, j.ID)
	if got.Status != StatusFailed {
		t.Fatalf("want failed, got %q", got.Status)
	}
	if got.StatusReason != "watchdog_timeout" {
		t.Fatalf("want reason watchdog_timeout, got %q", got.StatusReason)
	}
	if got.Error == nil || got.Error.Code != "TIMEOUT" {
		t.Fatalf("want TIMEOUT error, got %+v", got.Error)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/job/ -run TestRunJobWatchdogTimeout -v`
Expected: FAIL（`Stability`、`executor`、新 `NewWorker` 签名未定义）

- [ ] **Step 3: Refactor worker.go**

在 `internal/job/worker.go`：把 `Worker` 的 `svc *service.DownloadService` 字段改为 `exec executor`，并加 `Stability` 字段与接口：

```go
// Stability holds watchdog / cooldown durations for the worker.
type Stability struct {
	JobTimeout        time.Duration
	HeartbeatTimeout  time.Duration
	RateLimitCooldown time.Duration
}

// executor decouples the worker from DownloadService for testability.
// *service.DownloadService satisfies it; tests inject a fake.
type executor interface {
	SetClient(c *geektime.Client)
	Client() *geektime.Client
	ExecuteDownload(ctx context.Context, req service.DownloadRequest, reporter progress.Reporter) (geektime.Course, string, error)
}

type Worker struct {
	store          *Store
	exec           executor
	clientProvider func() *geektime.Client
	stability      Stability
	pausedAuth     atomic.Bool

	mu          sync.Mutex
	activeJobID string
	cancel      context.CancelFunc
	notify      chan struct{}
}
```

import 块补 `"time"`。改 `NewWorker`：

```go
func NewWorker(store *Store, exec executor, clientProvider func() *geektime.Client, st Stability) *Worker {
	return &Worker{
		store: store, exec: exec, clientProvider: clientProvider,
		stability: st,
		notify:    make(chan struct{}, 1),
	}
}

func (w *Worker) jobTimeoutOrDefault() time.Duration {
	if w.stability.JobTimeout > 0 {
		return w.stability.JobTimeout
	}
	return 60 * time.Minute
}
```

`runJob` 改造：把 `jobCtx, cancel := context.WithCancel(parent)` 改为 `context.WithTimeout(parent, w.jobTimeoutOrDefault())`；把 `w.svc.SetClient(...)`/`w.svc.Client()`/`w.svc.ExecuteDownload(...)` 全部改为 `w.exec.*`（接口已含 `SetClient`/`Client`/`ExecuteDownload`）；在 `err != nil` 块的 `apiErr := apperr.MapError(err)` 之前插入硬超时分支：

```go
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			apiErr := apperr.MapError(err)
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusFailed, "watchdog_timeout", apiErr)
			return
		}
		apiErr := apperr.MapError(err)
		switch apiErr.Code {
		// ... 原有 switch 不变 ...
```

import 块补 `"errors"`。

- [ ] **Step 4: Update serve.go call site**

`cmd/serve.go` 把：

```go
		worker := job.NewWorker(store, dlSvc, authMgr.GetClient)
```

改为：

```go
		worker := job.NewWorker(store, dlSvc, authMgr.GetClient, job.Stability{
			JobTimeout:        cfg.JobTimeout,
			HeartbeatTimeout:  cfg.HeartbeatTimeout,
			RateLimitCooldown: cfg.RateLimitCooldown,
		})
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/job/ -v && go build ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/job/worker.go internal/job/worker_test.go cmd/serve.go
git commit -m "feat(job): watchdog hard timeout via context.WithTimeout + executor interface"
```

---

### Task 9: job/worker — 限流全局冷却 + 心跳/自动恢复 ticker

**Files:**
- Modify: `internal/job/worker.go`
- Test: `internal/job/worker_test.go`（追加）

**Interfaces:**
- Produces: `Worker.rateLimitUntil atomic.Int64`；`runJob` 的 `CodeRateLimited` 分支设冷却；`loop` 顶部冷却闸门；`stabilityLoop` ticker（60s）执行 `MarkStaleJobs` + 冷却到期 `ResumeRateLimitJobs`。

- [ ] **Step 1: Write failing tests**

追加到 `internal/job/worker_test.go`：

```go
func TestRateLimitCooldownGate(t *testing.T) {
	store := newTestStoreTB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWorker(store, &fakeExecutor{err: nil}, nil, Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: 200 * time.Millisecond,
	})

	// 模拟一次限流：置冷却到 200ms 后
	w.applyRateLimitCooldown()
	if w.rateLimitUntil.Load() == 0 {
		t.Fatal("rateLimitUntil should be set")
	}
	if !w.cooldownActive() {
		t.Fatal("cooldown should be active immediately after rate limit")
	}

	// 等冷却过期
	time.Sleep(300 * time.Millisecond)
	if w.cooldownActive() {
		t.Fatal("cooldown should have expired")
	}
}

func TestRateLimitCooldownGrows(t *testing.T) {
	store := newTestStoreTB(t)
	w := NewWorker(store, &fakeExecutor{}, nil, Stability{RateLimitCooldown: 100 * time.Millisecond})
	w.applyRateLimitCooldown()
	first := w.rateLimitUntil.Load()
	w.applyRateLimitCooldown()
	second := w.rateLimitUntil.Load()
	if second-first < int64(100*time.Millisecond) {
		t.Fatal("cooldown should grow on repeated rate limits")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/job/ -run 'TestRateLimitCooldown' -v`
Expected: FAIL（`applyRateLimitCooldown`/`cooldownActive`/`rateLimitUntil` 未定义）

- [ ] **Step 3: Implement**

在 `Worker` struct 加字段：

```go
	rateLimitUntil atomic.Int64 // unix nano when global rate-limit cooldown ends; 0 = none
	cooldownStep   atomic.Int64
```

加方法：

```go
const rateLimitCooldownCap = 30 * time.Minute

func (w *Worker) rateLimitCooldownOrDefault() time.Duration {
	if w.stability.RateLimitCooldown > 0 {
		return w.stability.RateLimitCooldown
	}
	return 120 * time.Second
}

func (w *Worker) applyRateLimitCooldown() {
	step := w.cooldownStep.Load()
	base := int64(w.rateLimitCooldownOrDefault())
	shift := step
	if shift > 2 {
		shift = 2 // cap at 4x (120s->240s->480s)
	}
	cooldown := base << shift
	if cooldown > int64(rateLimitCooldownCap) {
		cooldown = int64(rateLimitCooldownCap)
	}
	w.rateLimitUntil.Store(time.Now().UnixNano() + cooldown)
	w.cooldownStep.Add(1)
}

func (w *Worker) cooldownActive() bool {
	until := w.rateLimitUntil.Load()
	if until == 0 {
		return false
	}
	return time.Now().UnixNano() < until
}
```

`loop` 顶部，在 `pausedAuth` 检查之后、`NextPendingJob` 之前，加冷却闸门：

```go
		if w.cooldownActive() {
			residue := time.Until(time.Unix(0, w.rateLimitUntil.Load()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(residue):
			}
			continue
		}
```

`runJob` 的 `CodeRateLimited` case 改为：

```go
		case apperr.CodeRateLimited:
			w.applyRateLimitCooldown()
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusWaitingRateLimit, apiErr.Message, apiErr)
```

加 `stabilityLoop` 并在 `Start` 中启动：

```go
// Start runs the worker loop until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	go w.loop(ctx)
	go w.stabilityLoop(ctx)
}

func (w *Worker) stabilityLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hb := w.stability.HeartbeatTimeout
			if hb <= 0 {
				hb = 10 * time.Minute
			}
			_ = w.store.MarkStaleJobs(ctx, hb)
			if w.rateLimitUntil.Load() != 0 && !w.cooldownActive() {
				if _, _ = w.store.ResumeRateLimitJobs(ctx); true {
					w.rateLimitUntil.Store(0)
					if step := w.cooldownStep.Load(); step > 0 {
						w.cooldownStep.Store(step / 2)
					}
					w.Enqueue()
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/job/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/job/worker.go internal/job/worker_test.go
git commit -m "feat(job): rate-limit global cooldown gate + heartbeat/auto-recover ticker"
```

---

### Task 10: video + course — 透传 reporter/segmentTimeout + per-segment 进度 + jitter

**Files:**
- Modify: `internal/video/video.go`（5 个函数签名 + download 循环回调 + DownloadMP4 签名）
- Modify: `internal/course/downloader.go`（video 调用处补参 + jitter 重写）
- Modify: `internal/video/video_test.go`（T6 的 `TestDownloadMP4ReturnsErrorOn403` 补 segmentTimeout 参数）
- Test: `internal/course/downloader_test.go`（新建，测 jitter）

**Interfaces:**
- Produces: video 函数新增 `segmentTimeout time.Duration` 与 `reporter progress.Reporter`（`articleID` 已有，透传至 `download`）；`download` ts 循环每段调 `reporter.OnArticleProgress(articleID, i+1, len(tsFileNames))`；`course.jitterMillis(interval int, rnd *rand.Rand) int`。

> 约束：`reporter == nil` 时跳过回调（CLI FSM 行为不变）。

- [ ] **Step 1: Write failing test for jitter**

新建 `internal/course/downloader_test.go`：

```go
package course

import (
	"math/rand"
	"testing"
)

func TestJitterMillisRange(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		got := jitterMillis(1, rnd) // interval=1s → [1000, 2000)
		if got < 1000 || got >= 2000 {
			t.Fatalf("jitter out of [1000,2000): %d", got)
		}
	}
	// interval=0 仍保证至少 1s 抖动窗口
	if got := jitterMillis(0, rnd); got < 1000 || got >= 2000 {
		t.Fatalf("interval=0 jitter out of range: %d", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/course/ -run TestJitterMillisRange -v`
Expected: FAIL（`jitterMillis` 未定义）

- [ ] **Step 3: Rewrite waitRandomTime + add jitterMillis**

`internal/course/downloader.go` 把 `waitRandomTime` 改为：

```go
// waitRandomTime waits interval seconds plus a 1x interval jitter window.
func (d *CourseDownloader) waitRandomTime() {
	time.Sleep(time.Duration(jitterMillis(d.cfg.Interval, d.waitRand)) * time.Millisecond)
}

// jitterMillis returns a sleep window in milliseconds of [interval*1000, interval*1000*2).
// When interval <= 0, a 1s base is used so requests are still spaced.
func jitterMillis(interval int, rnd *rand.Rand) int {
	base := interval * 1000
	if base <= 0 {
		base = 1000
	}
	return base + rnd.Intn(base)
}
```

- [ ] **Step 4: Thread segmentTimeout + reporter through video functions**

`internal/video/video.go` 改签名（import 块补 `"github.com/nicoxiang/geektime-downloader/internal/progress"`）：

```go
func DownloadArticleVideo(ctx context.Context, client *geektime.Client, articleID int, sourceType int, projectDir string, quality string, concurrency int, segmentTimeout time.Duration, reporter progress.Reporter) error {
	// ... body unchanged until downloadAliyunVodEncryptVideo call ...
	err = downloadAliyunVodEncryptVideo(ctx, client, playAuth, articleInfo.Data.Info.Title, projectDir, quality, articleInfo.Data.Info.Video.ID, concurrency, segmentTimeout, articleID, reporter)
	// ...
}

func DownloadEnterpriseArticleVideo(ctx context.Context, client *geektime.Client, articleID int, projectDir string, quality string, concurrency int, segmentTimeout time.Duration, reporter progress.Reporter) error {
	// ... downloadAliyunVodEncryptVideo(..., segmentTimeout, articleID, reporter) ...
}

func DownloadUniversityVideo(ctx context.Context, client *geektime.Client, articleID int, currentProduct geektime.Course, projectDir string, quality string, concurrency int, segmentTimeout time.Duration, reporter progress.Reporter) error {
	// ... downloadAliyunVodEncryptVideo(..., segmentTimeout, articleID, reporter) ...
}

func downloadAliyunVodEncryptVideo(ctx context.Context, client *geektime.Client, playAuth, videoTitle, projectDir, quality, videoID string, concurrency int, segmentTimeout time.Duration, articleID int, reporter progress.Reporter) error {
	// ... unchanged until download(...) ...
	return download(ctx, tsURLPrefix, videoTitle, projectDir, tsFileNames, []byte(decryptKey), playInfo.Size, isVodEncryptVideo, concurrency, segmentTimeout, articleID, reporter)
}

func DownloadMP4(ctx context.Context, title, projectDir string, mp4URLs []string, overwrite bool, segmentTimeout time.Duration) (err error) {
	// ... 把 downloader.DownloadFileConcurrently(ctx, dst, mp4URL, headers, 5, 0) 的 0 改为 segmentTimeout ...
}
```

> 实现者：按上方签名逐函数修改，函数体除调用点透传外保持不变；`DownloadMP4` 不接收 reporter（内嵌 mp4 不上报 per-seg）。

`download` 函数签名与 ts 循环回调：

```go
func download(ctx context.Context, tsURLPrefix, title, projectDir string, tsFileNames []string, decryptKey []byte, size int64, isVodEncryptVideo bool, concurrency int, segmentTimeout time.Duration, articleID int, reporter progress.Reporter) (err error) {
	// ... setup unchanged ...
	for i, tsFileName := range tsFileNames {
		u := tsURLPrefix + tsFileName
		dst := filepath.Join(tempVideoDir, tsFileName)
		headers := make(map[string]string, 2)
		headers[geektime.Origin] = geektime.DefaultBaseURL
		headers[geektime.UserAgent] = geektime.DefaultUserAgent
		fileSize, err := downloader.DownloadFileConcurrently(ctx, dst, u, headers, concurrency, segmentTimeout)
		if err != nil {
			return translateDownloadErr(err)
		}
		addBarValue(bar, fileSize)
		if reporter != nil {
			reporter.OnArticleProgress(articleID, i+1, len(tsFileNames))
		}
	}
	// ... mergeTSFiles unchanged ...
}
```

- [ ] **Step 5: Update course call sites**

`internal/course/downloader.go` 所有 video 调用补参：

```go
// DownloadSingleVideoProduct
return video.DownloadArticleVideo(d.ctx, d.geektimeClient, articleID, sourceType, columnDir, d.cfg.Quality, d.concurrency, d.cfg.SegmentTimeout, d.progressReporter)

// downloadVideoArticle 三支
video.DownloadUniversityVideo(d.ctx, d.geektimeClient, article.AID, course, dir, d.cfg.Quality, d.concurrency, d.cfg.SegmentTimeout, d.progressReporter)
video.DownloadEnterpriseArticleVideo(d.ctx, d.geektimeClient, article.AID, dir, d.cfg.Quality, d.concurrency, d.cfg.SegmentTimeout, d.progressReporter)
video.DownloadArticleVideo(d.ctx, d.geektimeClient, article.AID, productType.SourceType, dir, d.cfg.Quality, d.concurrency, d.cfg.SegmentTimeout, d.progressReporter)

// downloadTextArticle 两处 DownloadMP4
video.DownloadMP4(d.ctx, article.Title, columnDir, []string{videoURL}, overwrite, d.cfg.SegmentTimeout)
video.DownloadMP4(d.ctx, article.Title, columnDir, videoURLs, overwrite, d.cfg.SegmentTimeout)
```

- [ ] **Step 6: Fix T6 test signature**

`internal/video/video_test.go` 的 `TestDownloadMP4ReturnsErrorOn403`：

```go
	err := DownloadMP4(context.Background(), "t", dir, []string{srv.URL}, false, time.Second)
```
（补 `time.Second` 末参；import 块补 `"time"`。）

- [ ] **Step 7: Run tests + build**

Run: `go test ./internal/course/ ./internal/video/ -v && go build ./...`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/video/video.go internal/video/video_test.go internal/course/downloader.go internal/course/downloader_test.go
git commit -m "feat(video,course): thread reporter/segmentTimeout, per-segment progress, wider jitter"
```

---

### Task 11: cmd/serve — 4 个稳定性 flag

**Files:**
- Modify: `cmd/serve.go`（`init()` 注册 Duration flag）

**Interfaces:**
- Produces: serve flag `--job-timeout`/`--heartbeat-timeout`/`--rate-limit-cooldown`/`--segment-timeout` 写入全局 `cfg`，供 worker（T8）与 course/downloader（T10）使用。校验已在 root.go `PreRunE` 调 `ValidateServeConfig`（T2）覆盖。

> 注：`mergeConfig` 的 `cfg := *base` 结构体拷贝已自动把 `SegmentTimeout` 透传到 per-job `CourseDownloader.cfg`，无需改 `service/download.go`。

- [ ] **Step 1: Add flags**

`cmd/serve.go` 的 `init()` 函数改为：

```go
func init() {
	serveCmd.Flags().StringVar(&serveCfg.addr, "addr", "127.0.0.1:8080", "HTTP listen address")
	serveCmd.Flags().StringVar(&serveCfg.apiKey, "api-key", "", "Bearer token for API authentication")
	serveCmd.Flags().StringVar(&serveCfg.dbPath, "db", "", "SQLite database path")

	serveCmd.Flags().DurationVar(&cfg.JobTimeout, "job-timeout", 60*time.Minute, "单 job 最长执行时间，超时由看门狗判 failed")
	serveCmd.Flags().DurationVar(&cfg.HeartbeatTimeout, "heartbeat-timeout", 10*time.Minute, "progress 停滞超过该时长标记 stale_progress")
	serveCmd.Flags().DurationVar(&cfg.RateLimitCooldown, "rate-limit-cooldown", 120*time.Second, "触发限流(451)后的全局冷却基数（指数增长封顶 30m）")
	serveCmd.Flags().DurationVar(&cfg.SegmentTimeout, "segment-timeout", 60*time.Second, "单文件分段下载的超时")

	rootCmd.AddCommand(serveCmd)
}
```

import 块补 `"time"`。

- [ ] **Step 2: Build + smoke**

Run: `go build ./... && go run . serve --help`
Expected: 构建通过；`--help` 输出含 4 个新 flag。

- [ ] **Step 3: Run full test suite**

Run: `go test ./internal/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add cmd/serve.go
git commit -m "feat(serve): add job/heartbeat/ratelimit/segment timeout flags"
```

---

## Self-Review（计划作者自检）

**1. Spec 覆盖：**

| Spec 项 | 任务 |
|---------|------|
| P0-1 文件下载超时+状态码+StatusError+retry 区分 | T4 |
| P0-2 修 DownloadMP4 吞错 | T6 |
| P0-3 getPlayInfo 状态码检查（抽 CheckStatus） | T5（CheckStatus）+ T6（getPlayInfo 使用） |
| P1-1 看门狗硬超时 + 心跳 | T8（硬超时）+ T9（心跳 ticker，`MarkStaleJobs`） |
| P1-2 限流全局冷却 + 自动恢复 | T9（冷却闸门+`stabilityLoop`）+ T7（`ResumeRateLimitJobs`） |
| P1-3 细粒度进度（`OnArticleProgress`+`Done/Total`+透传） | T3（接口/模型）+ T10（透传+调用） |
| P1-4 jitter 加大 | T10 |
| 配置 4 flag | T2（字段+校验）+ T11（flag） |
| apperr `DeadlineExceeded` | T1 |
| store `MarkStaleJobs` | T7 |

无遗漏。

**2. 占位符扫描：** 无 TBD/TODO；每步均含可执行代码或命令。

**3. 类型一致性：**
- `NewWorker(store, exec, clientProvider, Stability)` 在 T8/T9/serve 一致；`executor` 含 `SetClient/Client/ExecuteDownload`，`*service.DownloadService` 与 `fakeExecutor` 均满足。
- `DownloadFileConcurrently(..., segmentTimeout)`：T4 定义→T6 传 `0`→T10 传 `d.cfg.SegmentTimeout`/`DownloadMP4` 传 `segmentTimeout`，一致；T6 测试在 T10 补参。
- video 函数签名在 T10 统一新增 `segmentTimeout`+`reporter`（`articleID` 透传至 `download`）。
- `Stability` 三字段（`JobTimeout/HeartbeatTimeout/RateLimitCooldown`）在 T8 定义、T9 使用、T11 flag 写入，命名一致。

---

## Execution Handoff

计划已保存到 `docs/superpowers/plans/2026-07-16-download-stability.md`。两种执行方式：

**1. Subagent-Driven（推荐）** — 每个 Task 派一个全新 subagent 实现，任务间两阶段评审，迭代快。

**2. Inline Execution** — 在当前会话用 executing-plans 批量执行，带检查点评审。

选哪种？
