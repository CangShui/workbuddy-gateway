# DSH 历史回放与 GUI 环境说明兼容

关联 [Issue #24](https://github.com/CangShui/workbuddy-gateway/issues/24)
和 [Issue #21](https://github.com/CangShui/workbuddy-gateway/issues/21)。

现有规则只处理 system 的第一个文本块，且执行时间早于配置提示词注入。
因此 assistant 历史、后续文本块和 `systemPrompt.fallback/force` 仍可能带有
同一已知身份句；Anthropic 的文本块合并还会使后续块开头的位置丢失。
仅处理身份前缀也不会覆盖 DSH 的 GUI 环境说明。

本补丁只补齐以下边界：

- system / assistant 字符串及每个标准 `type: "text"` 块的已知身份前缀，
  沿用已有的中性替换和大小写匹配规则；前导空白及正文中段不参与前缀匹配。
- 对 GUI 已知的两句开头做精确匹配，只把第一句中的
  `the DeepSeek Harness Web GUI` 改成 `a Web GUI`。
  后续页面指代规则及功能说明保持原文，不依赖整个段落的版本或摘要。
- GUI 规则保留围栏、行内和缩进代码；反引号分隔符状态跨行保留。
- 三协议在 fallback / force 注入后执行共享处理；Messages 在 system / assistant
  文本块合并前额外执行同一幂等处理。

已知 GUI 开头来自 DeepSeek Harness 的公开包
[`@deepseek-ai/dsh-harness-dev-context`](https://github.com/deepseek-ai/deepseek-harness/tree/main/packages/context/harness-dev-context)。
实现也参考了
[dsh-connect-workbuddy v4.15.1 的说明](https://github.com/dingminhua/dsh-connect-workbuddy/releases/tag/v4.15.1)。

用户正文、工具结果、工具参数、reasoning 和图片块不参与新增规则。
日志只记录固定规则和命中数，不新增请求正文日志。
现有完整请求诊断开关、账号调度、SSE 转换和计费逻辑不变。
本补丁不改请求头、客户端签名或授权身份，也不增加收费重试。
若上游仍返回 11128，错误继续原样返回；这不是全部渠道限制的通用解决方案。

## 验证范围

基于上游 main `ceb2ac8` 先增加 HTTP 回归测试，24 组配置注入用例和
12 组历史回放用例在原代码上失败。修复后检查三协议的流式与聚合请求，
并覆盖代码边界、工具参数及结果关联、日志内容、重复处理和继续拒绝时不重试。

相关兼容逻辑已经在 fork 中验证并部署；本次上游移植只使用合成请求和本地
`httptest` 上游，不读取生产凭据或重放私人会话，不新增真实收费调用。
这不代表完整长期会话或之后的上游策略变化已全部验证。
