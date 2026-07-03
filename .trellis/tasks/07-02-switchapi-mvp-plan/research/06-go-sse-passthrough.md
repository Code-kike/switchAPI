# Research #06: Go SSE 透传转发（go-sse-passthrough）

- **Query**: 验证 Agent 直通转发的 Go 实现要点：ReverseProxy flush 时机、流式超时设置、请求体模型名重写、断开传播与 O(1) 内存 tee；并交付可运行原型
- **Scope**: mixed（Go stdlib 源码 + 官方 Release Notes + 本机可运行原型实测 + 本机 CLI 二进制勘察）
- **Date**: 2026-07-03
- **原型位置**: `.trellis/tasks/07-02-switchapi-mvp-plan/research/06-sse-prototype/`（go.mod、main.go、proxy.go、patch.go、upstream.go、main_test.go、run.sh）

---

## 结论

1. **`httputil.ReverseProxy` 完全胜任 SSE 直通，且逐事件即时 flush 大部分是"免费"的**（置信度：High）。
   stdlib 对 `Content-Type: text/event-stream`（Go 1.12 起）与 `ContentLength == -1`（即 chunked/未知长度，Go 1.16 起）的响应**自动强制立即 flush**，无视 `FlushInterval` 配置；Go 1.18 起用 `mime.ParseMediaType` 匹配，`text/event-stream; charset=utf-8` 也命中。仍建议显式设 `FlushInterval: -1` 作为保险（对小 JSON 错误响应无害）。上游状态码原样透传是默认行为（`rw.WriteHeader(res.StatusCode)`）。

2. **必须用 `Rewrite` 钩子，不用 `Director`**（置信度：High）。
   当前源码中 `Director` 已标注 *"Director is deprecated. Use Rewrite instead."*，并列出两个安全缺陷：恶意客户端可借 `Connection` 头把 Director 添加的头标记为逐跳头而剥除；入站 `X-Forwarded-*` 默认保留导致伪造。`Rewrite`（Go 1.20+）在调用前先剥离逐跳头与入站 `X-Forwarded-*`。Agent 是 127.0.0.1 环回代理，无需调用 `SetXForwarded()`。

3. **流式安全的超时铁律**（置信度：High）：
   - `Transport.ResponseHeaderTimeout` 只覆盖"写完请求→收到响应头"，**不含读响应体时间**，对 SSE 流本身安全；但注意：**非流式**请求（`stream:false`）上游要等全部生成完才回头部，可能数分钟——所以该值要么为 0，要么设得很大，要么按请求体里的 `stream` 标志条件化。
   - **绝不能设 `http.Client.Timeout`**：官方文档明确它"includes … reading the response body"且"will interrupt reading of the Response.Body"。ReverseProxy 直接使用 `RoundTripper`，天然绕开此坑，不要自行包一层 Client。
   - **Agent 自身的 `http.Server` 必须 `WriteTimeout = 0`**（它限定整个响应写完的期限，会掐断长 SSE），用小的 `ReadHeaderTimeout` + 适度 `IdleTimeout` 兜底。
   - 拨号/TLS 阶段超时（`DialContext Timeout`、`TLSHandshakeTimeout`）安全，应保留。

4. **字节保真与压缩**（置信度：High，机制层面）：`Transport.DisableCompression = true` 只阻止 Transport 在请求本无 `Accept-Encoding` 时**自行注入** `gzip` 并透明解压（透明解压会剥掉 `Content-Encoding`、改变到手字节）；客户端自带的 `Accept-Encoding` 仍原样透传。**推论（重要）**：若 App 自己声明了 gzip 且上游真的压缩了 SSE，透传字节仍保真，但 **usage tee 看到的是 gzip 字节流，解析必失败**。稳健做法：Agent 出站请求强制 `Accept-Encoding: identity`（HTTP 语义合法，App 侧无感知），或在 tee 内按 `Content-Encoding` 解压。本机勘察显示 Claude Code 二进制里同时存在 `"Accept-Encoding":"identity"`（6 处）与 gzip 变体字符串，无法断定线上默认值（置信度：Low），因此该防御**必须**做。

5. **HTTP/2 到上游自动可用**：`DefaultTransport.Clone()` 保留 `ForceAttemptHTTP2: true`，https 上游经 ALPN 自动走 h2；明文 http 上游只有 HTTP/1.1（stdlib 不做 h2c）。SSE 语义在两者下一致（本原型实测的是 h1.1 + chunked）（机制置信度：High；h2 路径未实测：Medium）。

6. **请求体模型名重写可以做到"仅改 model 字段、其余字节完全不动"，纯 stdlib 即可**（置信度：High，原型已证明）。
   用 `json.Decoder.Token()+InputOffset()` 定位**顶层** `model` 字符串值的精确字节区间后拼接替换（`patch.go`，约 60 行）：不重排键序、不动空白、不碰嵌套的同名键。配套修正：`Out.ContentLength = len(patched)`（Transport 以该字段写 CL，Header 里的值仅装饰）、`Out.TransferEncoding = nil`（把入站 chunked 请求归一为定长，实测通过）、设置 `GetBody` 供可重放场景。防护边界：请求带 `Content-Type` 非 JSON 或带 `Content-Encoding` 时不改写；超过缓冲上限（原型 32MB）时改为 `MultiReader(已读头, 剩余体)` 原样直通；**无重定向命中时零拷贝直通，完全不缓冲**。

7. **客户端断开会经 context 全链路传播到上游，长流内存 O(1)**（置信度：High，原型实测）。
   服务器侧文档保证"客户端连接关闭时取消请求 context"；ReverseProxy 用 `outreq := req.Clone(ctx)` 把入站 ctx 带到出站请求 → 上游读被中止。流拷贝用固定 32KB 缓冲（可换 `BufferPool`）。拷贝中途出错（含客户端跑路）ReverseProxy 会 `panic(http.ErrAbortHandler)`——这是**设计内行为**，由 net/http server 回收，不是 bug，只需注意日志噪声。usage tee 在 `Read` 与 `Close` 双路径 finalize：**中断流也能产出 partial usage 记录**（实测 `Done:false`）。

8. **原型实测结果：全部断言通过**（置信度：High）。
   50ms 间隔的 11 个 SSE 事件穿过代理后逐个到达、间隔保持 50.5–50.9ms（无批化），首事件 +1.3ms 即达；5000 事件、624KB 长流字节级一致，解析器行缓冲高水位仅 **194 字节**；断开传播 <2s 内被上游观测。开发机**没有安装 Go 工具链**（`go: command not found`）——已用临时下载的 go1.26.4 完成全部验证，但正式实现前需装机。

---

## 证据与来源

### A. flush 时机与 Rewrite/Director（源码 + Release Notes 双源）

`reverseproxy.go`（master，2026-07-03 取回）关键代码：

```go
// FlushInterval 字段文档（L141-151）：
// "A negative value means to flush immediately after each write to the client.
//  The FlushInterval is ignored when ReverseProxy recognizes a response as a
//  streaming response, or if its ContentLength is -1; for such responses,
//  writes are flushed to the client immediately."

func (p *ReverseProxy) flushInterval(res *http.Response) time.Duration { // L666
    resCT := res.Header.Get("Content-Type")
    if baseCT, _, _ := mime.ParseMediaType(resCT); baseCT == "text/event-stream" {
        return -1 // negative means immediately
    }
    if res.ContentLength == -1 { return -1 }
    return p.FlushInterval
}
```

- 版本演进（对 golang/go 标签源逐一比对验证）：
  - go1.11 无此逻辑；**go1.12** 引入 `text/event-stream` 精确匹配立即 flush（当时 `ContentLength == -1` 仅是 TODO 注释）；
  - **go1.16** 增加 `res.ContentLength == -1 → -1`，与 Go 1.16 Release Notes 印证：*"ReverseProxy now flushes buffered data more aggressively when proxying streamed responses with unknown body lengths."* [Go Team, 2021, https://go.dev/doc/go1.16]
  - **go1.18** 改用 `mime.ParseMediaType`（go1.17 尚无），从此 `text/event-stream; charset=utf-8` 也触发。
- `Director` 弃用与安全缺陷：master L185-200 注释原文 *"Director is deprecated. Use Rewrite instead. This function is insecure…"*；Go 1.20 Release Notes：*"The Rewrite hook … supersed[es] the previous Director hook … permits Rewrite hooks to avoid certain scenarios where a malicious inbound request may cause headers added by the hook to be removed before forwarding. See issue #50580."* [Go Team, 2023, https://go.dev/doc/go1.20]
- `ProxyRequest.SetURL`（L43-60）：拼接 base path + 重写出站 Host；`SetXForwarded`（L62-98）为可选。
- 状态码透传：`rw.WriteHeader(res.StatusCode)`（L588）。
- 逐跳头按 RFC 自动剥离（L656-661），`ModifyResponse` 在剥离后调用（L163-176 注释）。

来源：[Go Team, master 源码, https://raw.githubusercontent.com/golang/go/master/src/net/http/httputil/reverseproxy.go]；渲染版 [Go Team, https://pkg.go.dev/net/http/httputil#ReverseProxy]；各标签版 `https://raw.githubusercontent.com/golang/go/go1.{11,12,16,17,18}/src/net/http/httputil/reverseproxy.go`。

### B. 超时与压缩（net/http 源码文档注释，均为官方权威）

- `transport.go` L231-235：`ResponseHeaderTimeout` — *"the amount of time to wait for a server's response headers after fully writing the request … **This time does not include the time to read the response body.**"*
- `client.go` L92-107：`Client.Timeout` — *"The timeout **includes** connection time, any redirects, and **reading the response body**. The timer remains running after Get/Head/Post/Do return and **will interrupt reading of the Response.Body**."*
- `transport.go` L199-207：`DisableCompression` — *"prevents the Transport from requesting compression with an 'Accept-Encoding: gzip' request header **when the Request contains no existing Accept-Encoding value**. If the Transport requests gzip on its own and gets a gzipped response, it's transparently decoded … if the user explicitly requested gzip it is not automatically uncompressed."*
- `transport.go` L47-55：`DefaultTransport` 自带 `ForceAttemptHTTP2: true`（`Clone()` 保留）；L301-306 `ForceAttemptHTTP2` 文档。
- `server.go` L3126-3131：`WriteTimeout` — *"the maximum duration before timing out **writes of the response** … It is reset whenever a new request's header is read."* → SSE 场景必须为 0；`ReadHeaderTimeout`/`IdleTimeout` 语义见 L3118-3137。

来源：[Go Team, master 源码 transport.go/client.go/server.go/request.go]；渲染版 [Go Team, https://pkg.go.dev/net/http#Transport / #Client / #Server]。

### C. 断开传播与 O(1) 拷贝（源码 + 原型实测双源）

- `request.go` L354-356：*"For incoming server requests, the context is canceled when the client's connection closes, the request is canceled (with HTTP/2), or when the ServeHTTP method returns."*
- `reverseproxy.go` L408 `ctx := req.Context()` → L434 `outreq := req.Clone(ctx)`：入站取消原生传播到出站。
- L712-714：拷贝缓冲 `make([]byte, 32*1024)`，可用 `BufferPool` 复用 → 转发路径内存 O(1)。
- L590-601：拷贝错误（上游断/客户端断）→ `panic(http.ErrAbortHandler)`（Issue 23643 设计决定）；L592/L602 保证 `res.Body.Close()` 必被调用 → tee 的 `Close` finalize 路径覆盖异常终止。
- L435-437：`ContentLength == 0` 时 `outreq.Body = nil`（GET 等无体请求自动跳过重写路径）。

### D. 本机勘察（READ ONLY）

- `go version` → **`go: command not found`**；`/usr/local/go`、snap、apt golang 均无 → 开发机无 Go 工具链（blocker，见下）。验证用工具链临时装于 `/tmp/gotoolchain/go`（go1.26.4 linux/amd64，来自 go.dev/dl 官方包）。
- Claude Code v2.1.199（`~/.local/share/claude/versions/2.1.199`，Bun 编译 ELF）`strings` 勘察：出现 `"Accept-Encoding":"identity"` ×6、`"Accept-Encoding","gzip,deflate"`、`identity,deflate,gzip` 等多种字面量，属不同内嵌 SDK/库；**无法确定 Messages API 实际线上值**（置信度：Low）。
- Codex（`~/.codex/packages/standalone/current/bin/codex`，Rust/reqwest）：存在 `accept-encoding` 头名与 `gzip, deflate`、`zstd` 特性字符串，同样不能下定论（置信度：Low）。
- → 结论 4 的"强制 identity 或 tee 解压"防御与此不确定性正交地稳健。

### E. 原型与实测输出（最强证据：行为验证于 go1.26.4）

原型结构（`06-sse-prototype/`）：
| 文件 | 作用 |
|---|---|
| `proxy.go` | 被测转发器：Rewrite + FlushInterval=-1 + 定制 Transport + auth 换头 + 模型重写 + usage tee |
| `patch.go` | 纯 stdlib 字节级 `model` 值拼接替换（Token+InputOffset 定位） |
| `upstream.go` | 假上游：50ms 节奏发 Anthropic 风格 SSE；记录收到的请求与写出的每个字节；观测 ctx 取消 |
| `main.go` | 演示：代理 127.0.0.1:19527 → 上游 127.0.0.1:19528（按研究项指定端口） |
| `main_test.go` | 4 组断言（时序不批化 / 字节级重写 / O(1) tee / 断开传播） |
| `run.sh` | `GO_BIN=… ./run.sh` 一键 vet+test+demo |

`go test -v`（另 `-race` 亦通过，`go vet`、`gofmt` 干净）：

```
--- PASS: TestChunkTimingPreservedNoBatching (0.46s)
--- PASS: TestModelRewriteByteExact (0.02s)
    --- PASS: TestModelRewriteByteExact/fixed-length_request
    --- PASS: TestModelRewriteByteExact/chunked_request_normalized
    --- PASS: TestModelRewriteByteExact/no_redirect_match_passes_through_byte-identical
--- PASS: TestUsageTeeO1MemoryOnLongStream (0.13s)
    main_test.go:246: stream=624275 bytes, events=5003, parser high-water=194 bytes
--- PASS: TestClientDisconnectPropagatesUpstream (0.12s)
    main_test.go:281: partial usage on disconnect: {Model:claude-haiku-4-5 InputTokens:123
        OutputTokens:1 CacheWrite:7 CacheRead:11 Status:200 Done:false HighWater:194}
PASS  ok  sseproto  0.738s
```

`go run .` 演示输出（逐事件到达时刻与间隔，穿过代理后 50ms 节奏原样保留、无批化；模型名字节级替换；Content-Length 修正；usage 解析完整）：

```
status=200 content-type=text/event-stream
event  1  +   1.3ms  gap   0.0ms  event: message_start
event  2  +  52.1ms  gap  50.8ms  event: content_block_delta
event  3  + 103.0ms  gap  50.9ms  event: content_block_delta
event  4  + 153.8ms  gap  50.8ms  event: content_block_delta
event  5  + 204.4ms  gap  50.7ms  event: content_block_delta
event  6  + 255.2ms  gap  50.7ms  event: content_block_delta
event  7  + 306.1ms  gap  50.9ms  event: content_block_delta
event  8  + 356.9ms  gap  50.8ms  event: content_block_delta
event  9  + 407.6ms  gap  50.7ms  event: content_block_delta
event 10  + 458.1ms  gap  50.5ms  event: message_delta
event 11  + 458.3ms  gap   0.2ms  event: message_stop

upstream saw: Content-Length=109 TransferEncoding=[] X-Api-Key=…-key
upstream body: {"model": "claude-haiku-4-5", "stream": true, "max_tokens": 64, "messages": [{"role":"user","content":"hi"}]}
parsed usage: {Model:claude-haiku-4-5 InputTokens:123 OutputTokens:42 CacheWrite:7 CacheRead:11 Status:200 Done:true HighWater:194}
```

（注：请求原文为 `{"model": "claude-3-5-haiku-20241022", …}`，重写后仅 model 值变化、含空格的原始排版逐字节保留；上游收到的 Content-Length=109 与补丁后长度一致；X-Api-Key 为原型假 key，按规约只显示末 4 位。）

复现方式：`GO_BIN=/path/to/go ./run.sh`（无外部依赖，`go >= 1.22`）。

---

## 对 design.md 的影响

design.md §4 标注 `[研究#6]` 的假设逐条对照：

| design.md 假设 | 判定 |
|---|---|
| "转发用 `httputil.ReverseProxy` 定制" | **confirmed** — 且指定用 `Rewrite` 钩子（Director 已弃用，见下） |
| "SSE 逐事件 flush、禁用缓冲" | **confirmed** — stdlib 对 event-stream/未知长度自动立即 flush；`FlushInterval:-1` 兜底；原型证明 50ms 粒度无批化 |
| "透传上游状态码" | **confirmed** — 默认行为 |
| "可选模型名重定向（请求体仅改 model 字段，其余字节不动）" | **confirmed（含实现约束）** — 纯 stdlib 字节级替换可行且已被原型证明；但必须补齐边界条件（见增补 3） |
| §10 "转发器集成测试（httptest 伪上游回放 SSE…）" | **confirmed** — 原型即该测试的雏形，可直接迁移 |

**需要写入 design.md 的增补**（不推翻任何决策，属实现约束细化）：

1. §4 转发条目改为明确 "`Rewrite` 钩子（非 `Director`）+ `FlushInterval: -1`"。
2. 新增 Transport 规格：`DefaultTransport.Clone()` 基础上 `DisableCompression=true`、`MaxIdleConnsPerHost≈32`（默认 2，并发请求下连接churn）、`ResponseHeaderTimeout=0`（或按 `stream` 标志条件化——非流式长生成头部可迟数分钟）、保留拨号/TLS 超时；**出站强制 `Accept-Encoding: identity`（或 tee 按 Content-Encoding 解压），否则上游压缩时 usage tee 失明**。
3. 模型重写边界：仅顶层 `model`；带 `Content-Encoding` 或非 JSON Content-Type 的请求体不改写；重写后 `ContentLength` 字段修正 + `TransferEncoding=nil`（chunked 归一）+ `GetBody`；超上限（建议 32MB）不缓冲直通；无命中零拷贝直通。
4. Agent 自身 HTTP 服务：`WriteTimeout=0`（否则掐断长流）、`ReadHeaderTimeout` 小值、`IdleTimeout` 适度。
5. 断流语义：客户端断开 → ctx 取消传播上游 + `ErrAbortHandler` panic（正常路径）；usage tee 必须在 `Close` finalize，产出 partial 记录（`Done=false`）——与 §4 "用量采集" 及研究#3 对接：**中断流缺失末尾 `message_delta`，output_tokens 不完整，计价需定义处理策略**（记 0/记最后已知值/标注不完整）。
6. §5 MVP 验收 "流式响应无可感知卡顿" 可加量化基线：原型显示代理引入的逐事件延迟 <1ms。

---

## 遗留不确定性

1. **CC/Codex 实际发送的 `Accept-Encoding`**（Low）：二进制字符串勘察不能定论，需在研究#1/#2 的实测环节抓真实请求确认；已用"强制 identity"设计使其不再影响正确性。
2. **h2 上游路径未实测**：原型走 h1.1+chunked；https 上游的 h2 SSE 行为由 stdlib 保证（ALPN 自动协商）但未做端到端验证，建议 M1 集成测试补一条 TLS 用例。
3. **OpenAI Responses 的超长 SSE 行 vs tee 行缓冲上限**：原型 `maxSSELine=1MB` 并丢弃超限行；OpenAI `response.completed` 事件内嵌完整输出，长输出可能超 1MB → 若丢弃则 usage 丢失。研究#3 需确定：提高上限、或对该事件流式提取 usage 字段。Anthropic 的 usage 事件（message_start/message_delta）很小，无此问题。
4. **Anthropic 官方请求体大小上限**未能从官方文档核实（docs.claude.com 对当前网络区域封锁）；32MB 缓冲上限暂为保守设计值，不影响技术可行性。
5. **开发机无 Go 工具链**（`go: command not found`）：本研究已用 `/tmp` 临时 go1.26.4 完成全部验证，但进入 M1 实现前需正式安装（blocker 级环境项）。
6. 计时断言在高负载 CI 上可能抖动（阈值已放宽为 30ms/50ms 且允许 2 个间隔失真），必要时可调。

---

### 来源清单

- [Go Team, 2026 取回, https://raw.githubusercontent.com/golang/go/master/src/net/http/httputil/reverseproxy.go]（及 go1.11/1.12/1.16/1.17/1.18 标签版同路径）
- [Go Team, 2026 取回, https://raw.githubusercontent.com/golang/go/master/src/net/http/{transport,client,server,request}.go]
- [Go Team, https://pkg.go.dev/net/http/httputil#ReverseProxy]、[https://pkg.go.dev/net/http#Transport]（上述源码的渲染版）
- [Go Team, 2021, https://go.dev/doc/go1.16]（未知长度流式响应更积极 flush）
- [Go Team, 2023, https://go.dev/doc/go1.20]（Rewrite 钩子取代 Director，issue #50580）
- 本机实测：go1.26.4 官方工具链运行原型（`go vet` / `go test -v` / `go test -race` / `go run .` 输出见上）
- 本机勘察：Claude Code v2.1.199 ELF、Codex standalone 二进制 `strings`（READ ONLY）

---

## 补录：真实上游端到端实测（2026-07-03，M0 验收项）

原型新增 REAL_UPSTREAM 模式（`main.go` 读 `REAL_UPSTREAM/REAL_KEY/REAL_AUTH/REAL_MODEL` 环境变量；`proxy.go` 增加 `bearerAuth` 开关支持 `Authorization: Bearer` 注入，默认仍为 `X-Api-Key`，既有测试不受影响、回归全绿）。以用户当前生效中转站 `https://anyrouter.top`（Bearer 鉴权）+ `claude-haiku-4-5-20251001` 跑一次 `max_tokens=64` 流式对话：

- `status=200 content-type=text/event-stream`；完整事件序列 message_start → content_block_start/delta×3/stop → message_delta → message_stop 全部透传；
- 事件间隔原样保留（24.5ms / 21.2ms / 100.9ms），无聚批，代理引入延迟不可感知；
- 鉴权链验证：客户端本地 token（`Authorization: Bearer local-agent-token`）被剥离，上游收到中转站真实 key；
- usage tee 从真实流解析：`{InputTokens:8 OutputTokens:16 CacheWrite:0 CacheRead:0 Done:true HighWater:392}`——O(1) 内存结论在真实上游复现；
- 消耗 24 token（≈忽略不计）。

对遗留不确定性的更新：#5（Go 工具链）**已解决**——工具链已固化到 `~/sdk/go1.26.4`（`PATH` 加 `$HOME/sdk/go1.26.4/bin` 即可）；#2（h2 上游）部分覆盖——https 上游 TLS+流式走通，但未打点 ALPN 协商结果，h2 专项断言仍留 M1 集成测试。

> 结论：implement.md M0 验收项「curl 经原型代理直连一个真实中转站完成一次流式对话」**通过**。
