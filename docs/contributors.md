# 贡献者

本项目接受社区通过 Pull Request 提交改进建议。以下贡献者的方案已被采纳并合入主线；
其中部分改动由维护者按当前代码基线手动应用，特此保留署名。

## 308532806

- GitHub: https://github.com/308532806
- 提议: https://github.com/CangShui/workbuddy-gateway/pull/1
- 标题: `feat: /v1/models 动态同步官方模型目录`
- 采纳内容: 提出「运行时从官方 CLI npm 包同步模型目录」的思路，用于解决 `/v1/models`
  列表硬编码、上游上新模型后无法自动跟进的问题。
- 落地方式: 以本仓库代码为基线重新实现为 `models.go`，并叠加低流量与容错策略
  （优先 unpkg 单文件约 20 KB、失败回退 npm tgz 流式提取并限制 80 MiB、
  失败版本指数退避、缓存带 schema 版本、与静态兜底合并去重）。
- 关联版本: v1.9.0 起

## WorldofPlainness

- GitHub: https://github.com/WorldofPlainness （亦使用 `Cooky0606` 提交）
- 提议: https://github.com/CangShui/workbuddy-gateway/pull/3
- 标题: `fix: normalize empty finish_reason in upstream stream chunks`
- 采纳内容: 上游在每个流式分片都下发 `finish_reason:""`，而 OpenAI 规范要求中间分片为
  `null`；Anthropic 协议翻译层会取第一个非 null 值作为最终 `stop_reason`，导致首个空串
  把 `stop_reason` 锁成 `end_turn`，真实终止分片的 `tool_calls` 不再被采纳，表现为
  Claude Code 工具不执行、终端无输出而网关日志显示成功。同时清理上游追加的旧版
  `function_call` 空壳。
- 落地方式: 按原方案应用，包含作者提供的 `stream_test.go`（分片归一化、空壳清除、
  走真实 `handleChatCompletions` 的端到端用例）。
- 关联版本: v1.11.0 起

## xiangshui001

- GitHub: https://github.com/xiangshui001
- 提议: https://github.com/CangShui/workbuddy-gateway/pull/11
- 标题: `fix: 加固本地网关鉴权、凭据保护与请求边界`
- 采纳内容:
  - 修复 npm 模型目录版本查询在响应体读取完成前取消请求上下文的问题。
  - 隐藏启动横幅中的 API Key。
  - 使用常量时间比较 API Key，保留本仓库原有的 Bearer、`x-api-key` 提取与回退行为。
  - 修正只读凭据校验：普通 HTTP 403 不直接确认 Token 失效；其他账号停用规则保持不变。
  - 凭据写入成功后再更新 `currAuth`，保留现有文件写入方式。
  - 采用日志秘密值脱敏的方向，按本仓库需求调整实现，保留错误码、失败原因与 TraceID；
    不改写用于业务判断的原始错误或客户端错误响应。
- 落地方式: 以当前代码为基线手工摘取兼容性风险较低的部分，并补充定向测试；
  未直接合并 PR 分支，也未采纳其中改变启动方式、请求边界或凭据文件替换方式的改动。
- 关联版本: v1.13.15 起（源码版本）

## MoRanYue

- GitHub: https://github.com/MoRanYue
- 提议: https://github.com/CangShui/workbuddy-gateway/issues/21
- 标题: `国际站 429 回退国内站后偶发 400/11128「Illegal API invocation from an unapproved channel」`
- 采纳内容: 通过真实出站请求抓包与二分定位，确认上游会对 system 消息开头的
  `You are an AI agent powered by DeepSeek Harness` 做渠道校验并返回 11128；
  同时验证了前导空白、正文中段和 user 消息不会触发。
- 落地方式: 新增 `sanitizeprefix.go`，在三个协议入口统一将 system 开头的该身份句
  等义改写为中性前缀，正文其余部分逐字保留；补充边界、数组 content、
  `developer` 归一化和三协议入口测试。
- 关联版本: v1.14.21 起
- 贡献者图: 通过 GitHub noreply 署名提交记录

## Ricardowindoy

- GitHub: https://github.com/Ricardowindoy
- 提议: https://github.com/CangShui/workbuddy-gateway/pull/23
- 标题: `fix(stream): 流式心跳 + 首字节前安全重放，缓解边缘节点静默期 RST`
- 采纳内容: 提出在等待上游首个有效 token 期间发送 SSE 注释心跳，并在客户端尚未收到
  内容时对首字节前 RST/EOF 做有限重放；同时给出静默期实测数据、配置项和重放边界。
- 落地方式: 按当前 `streamChatResponse` 结构重新实现，保留默认 15 秒心跳与最多一次
  重放。补齐响应体关闭、客户端取消区分、扫描器非网络错误拒绝重放，以及心跳已发送后
  重放失败只能以 SSE 错误事件收尾的处理；新增并发安全心跳和真实 HTTP 重放回归测试。
- 关联版本: v1.14.24 起
- 贡献者图: 通过 GitHub noreply Co-authored-by 署名提交记录

## GMagisk9527

- GitHub: https://github.com/GMagisk9527
- 提议: https://github.com/CangShui/workbuddy-gateway/pull/25
- 标题: `fix(compat): 补齐 DSH 历史、GUI 与配置提示词的 11128 兼容`
- 采纳内容: 将已知 DSH 身份前缀的净化扩展到 system/assistant 的每个标准文本块、
  配置 fallback/force 注入内容，并精确中性化 DSH GUI 环境句；跳过围栏、缩进和行内代码，
  不修改用户正文、工具参数、工具结果、reasoning 或图片。
- 落地方式: 接入当前三协议共用的 `prepareUpstreamChatRequest` 出站准备链，在文本块合并前
  和配置提示词注入后各执行一次幂等处理；保留 Messages 协议转换前的逐块处理。沿用
  PR 提供的回归测试并适配当前公共预处理结构。
- 关联版本: v1.14.24 起
- 贡献者图: 通过 GitHub noreply Co-authored-by 署名提交记录

## 说明

- 以上改动均以本仓库当时代码为基线应用，未直接合并原分支，但保留了作者署名与来源链接。
- 若你提交的方案被采纳但未出现在本文件，欢迎补充。
