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
- 邮箱: yl-77@qq.com
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

## 说明

- 以上改动均以本仓库当时代码为基线应用，未直接合并原分支，但保留了作者署名与来源链接。
- 若你提交的方案被采纳但未出现在本文件，欢迎补充。
