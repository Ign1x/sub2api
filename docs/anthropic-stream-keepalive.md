# Claude Code 流截断与非流式心跳

同步 `/messages` 不再使用 `gateway.nonstream_keepalive_interval` 的 JSON 空白心跳。
旧行为会在首个心跳时提交 HTTP 200，此后上游失败时，错误 JSON 仍随 200 返回，
严格的 Message 客户端会报 “JSON but not a Message”，并隐藏真实的上游错误。
现在无论等待响应头、切号还是聚合上游流，均等最终结果确定后再发送同步响应，
保留实际 HTTP 错误状态。其他同步协议仍沿用该配置。

给客户端发送心跳不会延长上游代理的等待预算。流式请求截断后，客户端改用非流式
重试，仍可能遇到上游超时。排查时应分别检查原始流是否包含结束事件，以及重试
过程中记录的上游状态，不能仅凭客户端的 HTTP 200 判断请求成功。

## 2.22.0 合并核查与补充修复

分支合并提交 `a584e6118` 保留了之前的 SSE 命名错误事件、普通转发的 EOF
最后一帧处理和账号级同步转流式。同步转流式默认关闭，升级版本不会自动开启；
仅凭界面版本号也无法判断运行二进制是否包含分支修复。

此前同步响应仅校验 JSON 语法，HTTP 200 中的 `{"type":"error",...}`、
`{}`、`null` 或其他协议的 JSON 仍可能被当作成功透传。现在普通 Anthropic、
API Key 透传和供应商原生 Anthropic 三条同步路径均要求顶层
`type:"message"` 且不带错误对象；不符合时，在写出正文前交给现有 502
错误策略与切号流程，保留原始响应体及上游请求 ID。

EOF 收尾同时覆盖 API Key 透传和供应商原生 Anthropic 路径。API Key 透传按完整
事件输出；干净 EOF 缺少空行时补齐分隔符，终态必须有合法且匹配的
`message_stop` JSON。单独的事件名、损坏 JSON 和读取中断均不能认定成功。

仅增加 Message 校验仍不能避免网关自己的错误被同步心跳包装成 HTTP 200。
因此补充在中间件入口排除 `/messages`，包括带前缀的兼容路由。外层代理需要
能够等待完整同步响应；长请求优先使用客户端流式，并由 SSE 心跳保活。
响应带 `server: cloudflare` 只能说明经过 Cloudflare，不能单凭该头确认错误来源。

2026-11-19 的 UltraNL 只读核查确认：运行版本为 `2.22.0-anthropic-json-r2`，
`GATEWAY_NONSTREAM_KEEPALIVE_INTERVAL=30`。两次大小相同的同步请求均因多个账号
返回 502/EOF 而失败；耗时 23.177 秒的请求向客户端返回 502，耗时 30.579 秒的
请求向客户端返回 200。Ops 两次均记录业务状态 502，并保存相同的 `type:error`
正文。这说明线上问题包含上游不可用与下游状态提前提交两部分。

## 等待响应头期间的流式心跳

`stream:true` 的 `/messages` 普通 Anthropic 与 API Key 透传路径，在物理 HTTP
请求等待响应头期间也发送 SSE 注释心跳，间隔沿用
`gateway.stream_keepalive_interval`。此前正文 reader 的心跳只有收到上游响应头后
才会启动；多个 429 重试和切号可能在 Cloudflare 的 120 秒窗口内一直保持静默。

心跳按请求累计时间调度，短尝试不会不断推迟首拍。每次 transport 返回前停止并
等待心跳协程退出，再让主协程接管 writer。心跳字节不计入生成内容，因此不阻止
原有安全重试；真实模型事件仍阻止重放。心跳写入失败后不启动新的账号尝试。

首拍之前的错误保留 HTTP 状态码和 JSON；首拍提交 200 后的 JSON/Data 错误
转为一份命名 `event: error`，业务状态仍记入 Ops。同步转流式聚合路径不启用
此客户端 SSE 心跳，继续使用原有同步 JSON 表示。

心跳解决等待期的下游静默，不会解除上游限流或恢复上游没有生成的内容。

## 账号级同步转流式

在 Anthropic 账号的新建、编辑或批量编辑中开启「同步请求转上游流式」。默认关闭，
保存于 `extra.anthropic_nonstream_to_stream`，无需数据库迁移。

开启后，客户端的同步 `/v1/messages` 请求以 `stream:true` 发往上游；网关收齐
`message_stop` 后返回一份 `application/json` Message。已有流式请求保持原行为。
文本、thinking 签名、工具参数、引用、上游附加字段及缓存用量随聚合结果保留。

响应缓冲受 `gateway.upstream_response_read_max_bytes` 限制，默认 128 MiB；沿用
流式转发的数据间隔超时、客户端取消和账号错误策略。未开始生成的 HTTP 错误可按
现有策略切换账号；已开始的生成若中断、参数损坏或超出缓冲上限，返回 JSON 错误，
记录已观测用量，不返回半截成功结果，也不重放该生成。

该开关缓解上游等待完整同步结果时的代理超时。上游即便使用流式仍长时间不响应时，
仍可能超时。客户端到网关的长同步等待需要外层代理允许足够长的等待时间；
同步 `/messages` 不发送 JSON 心跳，客户端可使用流式请求获得 SSE 保活。

## SSE 错误事件

`GatewayHandler.handleStreamingAwareErrorWithCode` 的 Anthropic SSE 错误带有
`event: error`。此前只有 `data: {"type":"error",...}`；Anthropic TypeScript SDK
会跳过没有事件名称的帧，因此真实上游错误被隐藏，Claude Code 可能只能看到截断。

合法错误帧示例：

```text
event: error
data: {"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}

```

## 正常 EOF 的最后一帧

原生 Anthropic 流解析器在干净 EOF 时处理最后一帧，并补齐 SSE 空行分隔符。
上游若发出了完整 `message_stop` JSON，却没有末尾空行，网关仍会向客户端送达
该结束事件，并保留最后一帧中的用量。

真实缺失的终态、损坏的终态 JSON 及上游读取错误仍按失败处理。网关不会补造
`message_stop`，也不会重放已经向客户端输出内容的请求。

## 验证与范围

- Go 回归覆盖错误事件名称、错误内容、已输出部分内容后的错误收尾、末尾空行缺失、
  损坏终态、读取错误及最后一帧用量。
- Anthropic TypeScript SDK 1.9.0 使用本地 mock 响应验证：旧帧被忽略，命名错误帧
  抛出带错误消息的 `APIError`。
- 回归覆盖同步 `/messages` 延迟错误的真实 HTTP 状态，以及同步转流式在响应头和
  正文等待期间不提前提交、完整返回 200、截断返回 502。
- `/messages` 已排除非流式 JSON 心跳；其他协议保留其 HTTP 200 提交语义。
  修复不能恢复上游丢失的生成内容，持续发生的上游截断或超时仍需由上游修复，
  或使用其他可用通道。
