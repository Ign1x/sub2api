# 缺少错误码的加密历史恢复

部分 OpenAI 兼容上游在 HTTP 400 中返回 `error.type=invalid_request_error`、空的 `error.code` 和固定的加密验证诊断，末尾可能附带 `[trace_id=<32 位十六进制>]`。旧版会显示脱敏错误，但不会进入加密恢复，所以新增渠道接收旧会话时会反复被拒。

原生 HTTP Responses 和尚未输出的 SSE 现在共享严格的错误解析：错误码缺失、null 或空字符串时，仅接受 `invalid_request_error`、空/缺失参数，以及完整固定诊断或精确密文定位诊断。仅移除已知格式的 trace 后缀；显式其他错误码、重复字段、冲突 envelope、普通加密措辞、脱敏文案和未知后缀均不授予恢复资格。

定位诊断接受 URL-safe Base64 尾部合法的一个或两个 `=` 填充；仍须唯一匹配本次输入，非法或中间填充不接受。400 Beta 转换历史 reasoning 时允许省略 Codex 的已知内部 turn/provenance 元数据 `internal_chat_message_metadata_passthrough`，并保留可读推理、当前任务及工具结果。其他未知 reasoning 字段仍停止恢复。

解析只提供错误证据，既有恢复边界继续生效。精确恢复最多在同账号重试一次，保留任务、推理摘要、工具参数及结果；多项或未知历史仍受原有定位、400 Beta、完整性及交付检查约束。不会修改客户端历史，也不改变 400/409 开关。400 Beta 仍可能省略无法读取的旧上下文。

本次修复提交到 `stable`，部署基于 2.24.3，并保留 UltraNL 原有的 Anthropic 修复。线上版本和备份位置以部署记录为准。
