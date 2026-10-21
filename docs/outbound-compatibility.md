# 多 Harness 出站兼容

三个原生入口在协议转换后共用同一条出站准备链：

- Chat Completions：`/v1/chat/completions`
- OpenAI Responses：`/v1/responses`
- Anthropic Messages：`/v1/messages`

目标是尽量对齐已验证的官方请求结构，同时保留客户端的指令、工具和参数语义。
**不是逐字复制官方私有 system prompt，也不承诺绕过服务端渠道校验。**
参数和请求头基于 2026-10-21 国内桌面实抓样本，不声称自动跟随官网最新版本。

## 开启和回退

**先部署包含本功能的新构建，再添加配置字段。旧二进制不认识 requestProfile，
可能拒绝新配置；本次代码完成不代表生产服务已经更新。**

在已有配置的 `upstream` 对象内增加以下字段，不要替换其他配置或凭据：

```json
{
  "upstream": {
    "requestProfile": "official"
  }
}
```

- 配置热加载后影响新模型请求，无需重启。
- 改为 `"legacy"` 可回退。字段省略、空字符串或 null 使用 legacy。
- 非法值或错误类型会被配置校验拒绝；热加载保留上一份有效配置，不会部分应用。
- 新示例配置显式使用 official；已有配置不会被程序自动改写。
- 一次逻辑请求只选择一次 profile，热加载不会改变在飞请求及其重试的压缩和头部策略。
- 本功能没有自动开启 `debug.fullRequests`。

## 官方兼容模式做什么

### 统一正文

1. 保留已有 developer→system、已知前缀净化、工具历史修复、推理历史修复及全局提示词规则。
2. 只合并**连续前导、无附加元数据的纯文本 system**，保留文本顺序，并在合并后追加配置的强制提示词。
   不跨越 user/assistant/tool 边界，不丢弃图片、name、cache_control 等元数据；
   无法无损合并时保留原结构。
3. 在缺少该字段时补 `stream_options.include_usage=true`，不覆盖显式 false。
4. 对已有完整样本的 `deepseek-v4.1-flash`：
   - 未指定 temperature，且未指定 top_p 时，补 temperature=1；
   - 未指定 verbosity 时，补 verbosity=high；
   - 未声明输出预算且模型目录还没有上限时，补已验证的 max_tokens=128000。
5. Chat 的 max_completion_tokens 在没有 max_tokens 时转为 max_tokens；
   两者同时存在时不擅自覆盖客户端值。客户端仍应避免传入互相冲突的预算。
6. 未知模型不猜测 temperature、verbosity 或输出预算上限。

`null` 视为未声明；有效的显式 `0`、`false`、较短输出预算及 low/high/max 均保留。
tool_choice 和 parallel_tool_calls 不因官方样本省略就被删除，工具名称、参数 schema 不被裁剪成官方的 23 个工具。
网关仍按原有逻辑向上游使用 SSE，为非流式客户端本地聚合。

### 补齐协议参数转换

这些转换修正不依赖 official 开关：

| 原协议字段 | 网关 Chat 字段 |
|---|---|
| Responses reasoning.effort | reasoning_effort，保留 max 等显式值 |
| Responses text.verbosity | verbosity |
| Responses parallel_tool_calls | parallel_tool_calls，保留 false |
| Responses max_output_tokens | max_tokens |
| Messages output_config.effort | reasoning_effort，优先于旧 thinking.effort 和预算推导 |
| Messages thinking.type=disabled | 继续关闭思考，不被 output_config 再次开启 |
| Messages tool_choice.disable_parallel_tool_use | parallel_tool_calls 的反值 |

不执行 `high→max` 的无条件升级。用户主动选 high、辅助标题任务使用 high，
与前置代理错误降档不是一回事。

### 国内模型请求头及压缩

仅国内模型 POST `/v2/chat/completions` 使用已采集的静态头：

- User-Agent：`WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0`
- Accept：`application/json`
- X-IDE-Type / X-IDE-Name / X-IDE-Version：`WorkBuddy / WorkBuddy / 5.8.3`
- X-Private-Data：`true`
- X-Codebuddy-Request：`1`
- X-Request-ID 与 X-Conversation-Message-ID 使用相同的 32 位消息 ID；
  同一逻辑请求的安全重试沿用该标识。

保留账号所属的 access token、用户/企业/域信息及会话标识；
模型请求不再携带旧版 X-Client-ID、X-Client-Version、Origin、Referer、
X-Refresh-Token 和缺省 X-No-* 头。
登录、刷新、额度、模型目录等控制接口保持原逻辑。

模型 JSON 使用 gzip 压缩，请求头与实际压缩字节、Content-Length 一致。
响应压缩交给 Go HTTP 客户端处理，不声明无法解码的 br/compress 等能力。

以下字段不凭空制造：

- X-Device-Token 等设备凭证；
- x-stainless / Node 运行时等不属于当前 Go 网关的 SDK 身份；
- 不能可靠判断的 Agent 类型、用途和业务增长事件；
- 未采集验证的 B3、ACP 等其他链路字段。

国际站的 UA、认证头、站点和请求编码保持 legacy，不把国内桌面头复制到国际站。
official 的正文准备规则仍适用于该请求；未知模型依旧不注入未经验证的采样字段。
国内模型 URL 当前仍沿用原有账号站点配置，未改成抓包中另一域名，不把近似兼容宣传为完全相同的官方链路。

## Codex、dsh、Claude Code 接入建议

尽量让客户端直接连接网关对应的原生协议入口，避免先经过另一个 Responses→Chat 或 Messages→Chat 翻译层。

- Codex / dsh 使用 Responses 时，可以原样提交 instructions、developer/user、工具与 reasoning.effort。
- Claude Code 等使用 Messages 时，可以原样提交 system、thinking、output_config 和 Anthropic 工具。
- 原生 Chat 客户端保留 system/developer 与 user 的区别。

当前 Responses 工具转译支持 function 工具。原生 custom/freeform/namespace、
托管搜索或其他专用工具协议不因开启此 profile 就自动获得完整支持；
客户端包含这些工具时，应先确认接入协议和工具封装，不能把本模式当作全量 OpenAI API 等价实现。

本轮诊断发现过“客户端 max，经 CPA 后到网关已是 high”和“本地 developer，到网关已是 user”。
网关不能从已经丢失的角色和档位可靠反推出原意：

- 不把所有 high 强制改为 max；
- 不因 user 文本里出现 `<app-context>`、Codex 或 Harness 关键词就提升为 system；
- 经过前置代理时，应同时修正其档位映射和角色翻译，不能只改本网关。

## 日志与排障

新增事件使用现有 TraceID，写入原有项目 `logs/`：

- `official_request_body_prepared`：补入哪些默认字段，不记录提示词正文；
- `official_instruction_messages_merged`：合并的前导指令数量；
- `upstream_request_profile_applied`：使用哪个 profile、是否采用国内头、压缩前后字节数；
- 配置切换与编码失败也有持久化审计。

只有显式开启 enabled 和 fullRequests 才记录完整脱敏 JSON。
在 gzip 模式下，完整出站日志明确标记：

```text
body_representation = decoded_json_before_content_encoding
content_encoding = gzip
wire_body_bytes = 实际压缩后长度
body_bytes = 未压缩JSON长度
```

因此能直接比对完整 messages/tools，而不会把 gzip 二进制误当 JSON。
这些是 HTTP 发送前快照，不是 TLS 抓包。排障后应关闭 fullRequests，完整业务正文依然敏感。

## 验证与边界

新增测试使用合成账号、指令和本地上游，不访问真实模型服务：

- 三协议 × 流式/聚合六种入口，生成相同语义输入的相同上游 JSON；
- 真实 HTTP 接收 gzip、usage 和与消息 ID 相等的请求 ID；
- high/max/off、显式 false/0、未知模型与短预算保留；
- 消息合并边界、图片及 schema 保留；
- 热加载 last-good、请求快照及安全重试；
- 只读入历史私有采集材料的可选离线重放。

```powershell
go test ./...
go test -race ./...
```

可选历史采集重放（不发网络请求）：

```powershell
$env:WORKBUDDY_COMPAT_CAPTURE = 'C:\private\captured-requests.private.json'
try { go test -run '^TestOfficialPrivateCaptureReplay$' -count=1 -v }
finally { Remove-Item Env:WORKBUDDY_COMPAT_CAPTURE }
```

fixture 必须是诊断采集器产生的、含 requests 数组的完整脱敏记录。
原始采集文件及官方私有提示词不提交仓库。通过本地测试不等于生产上游验收，
也不能保证服务端永远接受任何 Harness。

协议参数参考：
[OpenAI Responses 参数](https://developers.openai.com/api/reference/cli/resources/responses/methods/create)、
[Anthropic effort](https://platform.claude.com/docs/en/build-with-claude/effort)。
官方桌面默认值和静态头来自本机授权采集，不能由通用 API 文档替代。
