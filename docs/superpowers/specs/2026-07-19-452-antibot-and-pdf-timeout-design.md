# 452 反爬误判与 PDF 渲染超时修复设计

**日期：** 2026-07-19
**状态：** 已实现（commit 0dc8e5d, 8b49029, 978297a, 15f8385）
**适用范围：** `geektime-downloader serve` 本地 HTTP API 服务（API 层 + PDF/chromedp 层）

---

## 1. 背景与问题

Ubuntu 上用编译好的二进制跑 `serve`，agent 通过 HTTP API 批量下多门课程。现象：**每批次只有第 1 门成功，后续课程全部失败**，反复重试（run18-run24）结果一致。Agent 报告的错误是 `net::ERR_INVALID_AUTH_CREDENTIALS`。

### 1.1 根因（日志核实，已坐实）

日志 `~/.config/geektime-downloader/geektime-downloader.log` 显示三个关键事实：

1. **文章内容 API `/serv/v1/article` 全程 200** —— GCID/GCESS cookie 完全有效，认证没问题。
2. **课程查询 API `/serv/v3/column/info` 间歇性返回 452，且 response body 为空**，几分钟后同请求又 200（自愈）。例：`22:23:20` 452 → `22:26:42` 200；`07-04 14:50:40` 452 → `15:38:49` 200。452 与请求 burst 相关（07-04 14:34-14:48 连续 6 次查询后触发）。
3. **PDF 失败的真实错误是 `context deadline exceeded`**（`downloader.go:215`），时间差正好 60s = `--print-pdf-timeout` 默认值。即 chromedp 页面加载卡死到超时，不是认证错误。

### 1.2 两个独立缺陷

**缺陷 A —— 452 被误判为账号失效（核心缺陷，致 8 门全挂）：**

`internal/geektime/client.go:CheckStatus` 把 HTTP 452 映射成 `ErrAuthFailed`（"当前账户在其他设备登录或者登录已经过期"）。Worker（`internal/job/worker.go`）据此把 job 置 `WAITING_AUTH`，要求 agent 重新更新 cookie。但 cookie 根本没失效（v1/article 一直 200），agent 无论换不换 cookie 都没用 —— 这正是 run18-run24 一致失败的真因。

三个证据闭环：452 body 为空（真·账号失效会有 JSON 错误码 `-3050/-2000` 走 `do()` 的 code 分支，不是 HTTP 452）；452 自愈；452 与 burst 相关。**结论：452 是边缘反爬/限流拦截，不是账号失效。**

agent 报的 `ERR_INVALID_AUTH_CREDENTIALS` 是它对 `WAITING_AUTH`/`AUTH_EXPIRED` 状态的转述，根子在 452 误判。

**缺陷 B —— PDF 渲染超时：**

`internal/pdf/pdf.go` 用 `chromedp.Sleep(PrintPDFWaitSeconds)` 盲等页面加载后直接 `printToPDF`。文章 API 已 200 拿到内容，但 chromedp 渲染 `/column/article/{aid}` 页面卡到 60s 超时。原因可能是页面加载级反爬挑战或渲染未就绪。当前对页面加载失败毫无感知（限流监听只盯 `/serv/v1/article` 的 451），干等到 `context deadline exceeded`。

### 1.3 放大器

`--interval` 默认 1 秒，请求非常密集，是触发 burst 反爬的温床。

---

## 2. 目标与非目标

### 2.1 目标

- **A：** 452 不再当账号失效；走已有限流退避路径（resty 层重试 + worker 全局冷却自动 resume），cookie 不动。
- **B：** PDF 渲染用条件等待替代盲等，页面加载失败时快速失败并映射成限流退避，而非干等 60s 超时。
- 默认下载间隔调大，降低 burst 触发概率。

### 2.2 非目标（YAGNI）

- 不改 cookie 构造（仍只 GCID+GCESS，不主动捕获 SERVERID）。
- 不做"每 job 复用 browser"（日志显示 course 1 无 SERVERID 也能成功，非根因）。
- 不改 DB schema、不改 CLI FSM 行为、不改状态机。
- 不处理真·账号失效（code -3050/-2000）的语义，那条路径仍走 `ErrAuthFailed`。

---

## 3. 已决议的设计分叉

| 分叉 | 决议 | 理由 |
|------|------|------|
| 452 处理策略 | **HTTP 重试 + 限流退避** | resty 对 452/451 自动重试 2-3 次+退避；仍失败则走 worker 已有的 `WAITING_RATE_LIMIT` 指数退避（封顶 30m）自动 resume。复用 `2026-07-16-download-stability-design.md` 已建好的冷却机制，改动最小且行为与 451 一致。 |
| 452 语义 | **限流类，非账号失效** | 日志三证据闭环证明是临时反爬拦截。真账号失效有 JSON code 走另一条路径。 |
| PDF 等待 | **条件等待 + 加载失败监听** | `WaitReady` 文章正文节点替代 `Sleep`；监听 `network.EventLoadingFailed` 快速失败。 |
| interval 默认 | **1s → 3s** | 降低 burst 触发；serve 场景对吞吐不敏感。CLI 默认同步改（flag 默认值）。 |

---

## 4. 架构概览

```
geektime API do()
  └─ CheckStatus: 451→ErrGeekTimeRateLimit, 452→ErrGeekTimeRateLimit(改), code -3050/-2000→ErrAuthFailed(不变)
  └─ resty RetryCondition: 451/452 重试 3 次 + 退避(2s/4s/8s jitter)
       └─ 仍失败 → ErrGeekTimeRateLimit 冒泡
            └─ worker: CodeRateLimited → WAITING_RATE_LIMIT → 全局冷却(已有, 120s→240s→480s 封顶30m) → 自动 resume

PDF chromedp
  └─ tasks: network.Enable, setCookies, Navigate, WaitReady(正文节点), [comments actions], printToPDF
  └─ listener: network.EventLoadingFailed → 认证/失败快速返回 ErrGeekTimeRateLimit
  └─ (原 /serv/v1/article 451 监听保留)
```

---

## 5. 组件设计

### 5.1 缺陷 A-1：452 映射改限流（`internal/geektime/client.go`）

`CheckStatus` 的 switch：

```go
switch statusCode {
case 451, 452:
    return ErrGeekTimeRateLimit
...
}
```

将 452 从 `ErrAuthFailed` 改为 `ErrGeekTimeRateLimit`。语义统一为"被临时拦截，可重试"。

> 注：`do()` 中 code `-3050`/`-2000`（未登录/已失效）仍返回 `ErrAuthFailed`，这条真账号失效路径不变。

### 5.2 缺陷 A-2：resty 层 451/452 重试（`internal/geektime/client.go:NewClient`）

当前 `SetRetryCount(1)` 但无 `RetryConditions`，resty 默认只重试传输错误（已核实源码），不重试 HTTP 451/452。新增：

```go
restyClient := resty.New().
    SetCookies(cs).
    SetRetryCount(3).
    SetRetryWaitTime(2*time.Second).
    SetRetryMaxWaitTime(10*time.Second).
    SetHeader(RefererHeader, DefaultReferer).
    SetLogger(logger.DiscardLogger{}).
    AddRetryCondition(func(r *resty.Response, err error) bool {
        if err != nil {
            return true // 传输/超时错误重试
        }
        sc := r.StatusCode()
        return sc == 451 || sc == 452 || sc >= 500
    })
ApplyBrowserHeaders(restyClient)
```

重试 3 次，退避 2s→4s→8s（resty 内置指数+抖动，受 `RetryMaxWaitTime` 10s 封顶）。日志显示 452 几分钟内自愈，重试 + 退避大概率直接过，不必触发 worker 冷却。

> 重试发生在 `do()` 之前的 `request.Execute` 内，对调用方透明；`do()` 仍按最终响应 `CheckStatus`。重试期间 resty 日志被 `DiscardLogger` 丢弃，但 `do()` 的请求/响应日志保留，可观测最终结果。

### 5.3 缺陷 A-3：worker 错误码路由（`internal/job/worker.go`）

无需改动。`apperr.MapError` 已把 `geektime.ErrGeekTimeRateLimit` → `CodeRateLimited`（已核实 `apperr/errors.go`），worker `runJob` 的 `case apperr.CodeRateLimited:` 已走 `applyRateLimitCooldown()` + `WAITING_RATE_LIMIT` + 自动 resume（stability spec 已实现）。452 改映射后自动复用这条路径。

### 5.4 缺陷 B-1：PDF 条件等待（`internal/pdf/pdf.go`）

把 `chromedp.Sleep(time.Duration(cfg.PrintPDFWaitSeconds) * time.Second)` 替换为等待文章正文节点就绪：

```go
tasks := chromedp.Tasks{
    network.Enable(),
    chromedp.Emulate(device.IPadPro11),
    setCookies(cookies),
    chromedp.Navigate(geektime.DefaultBaseURL + `/column/article/` + strconv.Itoa(aid)),
    waitArticleReady(),  // 替代 Sleep
}
```

`waitArticleReady`：`chromedp.WaitVisible` 文章正文容器（极客时间文章页正文节点 class 前缀如 `Index_articleContent` 或等价选择器），带 `PrintPDFWaitSeconds` 作为上限超时（通过 `chromedp.WithTimeout` 或外层 `timeoutCtx` 兜底）。节点就绪即继续，不再盲等固定秒数。

> 若选择器因页面改版失效，`WaitVisible` 会等到 `timeoutCtx` 超时 → 行为退化为当前的超时失败，不会比现状更差。实现时用 `document.querySelector` 探测多个候选选择器，提高鲁棒性。

### 5.5 缺陷 B-2：PDF 加载失败监听（`internal/pdf/pdf.go`）

在现有 `listener` 的 `switch` 内新增 `*network.EventLoadingFailed` 分支：

```go
case *network.EventLoadingFailed:
    // 页面主文档或关键子资源加载失败（认证拒绝/被拦截）
    if isMainFrameOrAuth(responseReceivedEvent.RequestID, responseReceivedEvent.ErrorText) {
        logger.Warnf("Article page load failed, articleID: %d, error: %s, pdfFileName: %s",
            aid, responseReceivedEvent.ErrorText, pdfFileName)
        rateLimit = true  // 复用现有快速失败机制
        timeoutCancel()
        listenerCtxCancel()
        return
    }
```

`ErrorText` 含 `ERR_INVALID_AUTH_CREDENTIALS` / `ERR_BLOCKED` / `net::ERR_FAILED` 等时判定为反爬拦截，设 `rateLimit=true` 复用现有返回 `ErrGeekTimeRateLimit` 的路径（`pdf.go:124-128` 已有此分支）。这样页面加载失败秒级失败 + 映射限流退避，而非干等 60s 超时成 `context deadline exceeded`（INTERNAL_ERROR）。

> 主文档失败才触发（通过 `network.GetRequestTree` 或记录主 frame requestID）；图片/字体等子资源失败忽略，避免误杀。实现时记录 `Page.frameNavigated` 的主请求 ID 做匹配。

### 5.6 缺陷 C：默认 interval 调大（`cmd/root.go`）

`--interval` 默认 `1` → `3`（秒）。serve 批量场景对吞吐不敏感，3s 间隔显著降低 burst 反爬触发。`waitRandomTime` 的 jitter 逻辑（stability spec P1-4，`interval*1 ~ interval*2`）不变，实际间隔 3-6s。

---

## 6. 配置项

| flag | 原默认 | 新默认 | 用途 |
|------|--------|--------|------|
| `--interval` | `1` | `3` | 下载间隔基数（秒），降低 burst |

其余 flag（`--print-pdf-wait`/`--print-pdf-timeout`/`--rate-limit-cooldown` 等）不变。resty 重试次数（3）/退避（2s/10s）为代码常量，不暴露 flag（YAGNI）。

---

## 7. 错误映射（`internal/apperr/errors.go`）

无需改动。`ErrGeekTimeRateLimit` → `CodeRateLimited` 的映射已存在，452 改映射后自动生效。`context.DeadlineExceeded` → `CodeTimeout` 已在 stability spec 实现。

---

## 8. 测试策略

| 层级 | 内容 |
|------|------|
| `geektime/client_test.go` | httptest 返回 452：断言 `CheckStatus` 返回 `ErrGeekTimeRateLimit`（非 `ErrAuthFailed`）；断言 resty 重试 3 次后仍 452 才返回错误；452 后 200 断言重试成功 |
| `pdf` | 注入加载失败事件断言返回 `ErrGeekTimeRateLimit`；正文节点就绪断言不盲等（耗时 < PrintPDFWaitSeconds） |
| `job/worker` | 回归：`ErrGeekTimeRateLimit` → `WAITING_RATE_LIMIT` + 冷却（已有用例，确认 452 路径同行为） |
| CLI 回归 | FSM 模式行为不变；interval 默认 3 不破坏 CLI（仅默认值变化） |

---

## 9. 文件清单

```
internal/geektime/client.go        5.1 452→ErrGeekTimeRateLimit; 5.2 resty 重试
internal/geektime/client_test.go   452 映射 + 重试用例
internal/pdf/pdf.go                5.4 WaitReady 替代 Sleep; 5.5 EventLoadingFailed 监听
cmd/root.go                        5.6 --interval 默认 1→3
docs/superpowers/specs/2026-07-19-452-antibot-and-pdf-timeout-design.md  本 spec
```

---

## 10. 验证标准

- 复现场景：Ubuntu serve 批量下多门课程，第 2 门起不再因 452 判 `WAITING_AUTH`。
- 452 出现时：日志可见 resty 重试，多数情况下重试后 200 继续；仍失败则 `WAITING_RATE_LIMIT` 自动 resume，无需 agent 换 cookie。
- PDF 页面加载失败时：秒级返回 `RATE_LIMITED` 而非 60s 后 `TIMEOUT`。
- 文章 API 200 时 PDF 正常生成，无回归。
