# Claude Code 流截断与非流式心跳

`gateway.nonstream_keepalive_interval` 通过 JSON 空白保活客户端到 Sub2API 的连接。
空白本身不破坏 JSON，但首个心跳会提交 HTTP 200。此后上游失败时，错误 JSON
仍随 HTTP 200 返回，严格的 Message 客户端可能报 “JSON but not a Message”。

给客户端发送心跳不会延长上游代理的等待预算。流式请求截断后，客户端改用非流式
重试，仍可能遇到上游超时。排查时应分别检查原始流是否包含结束事件，以及重试
过程中记录的上游状态，不能仅凭客户端的 HTTP 200 判断请求成功。

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
仍可能超时。客户端到网关的长同步等待可继续配合非流式 JSON 心跳。

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
- 非流式心跳保留其 HTTP 200 提交语义。修复不能恢复上游丢失的生成内容，持续发生的
  上游截断或同步请求超时仍需由上游修复，或使用其他可用通道。
