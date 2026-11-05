# 测试

独立于 Go 包的测试集中到本目录，Go 白盒单元测试继续与生产包同目录。所有命令默认在项目根目录执行，测试不使用生产账号或模型生成接口。

## 统一入口

```powershell
.\scripts\test.ps1                 # 全部：Go 静态检查/单测、网页单测、模拟构建回归
.\scripts\test.ps1 -Suite go
.\scripts\test.ps1 -Suite web
.\scripts\test.ps1 -Suite build
```

- 使用 Go、Node.js 与 PowerShell；缺失所选测试需要的工具会报错，不静默跳过。
- Go 测试阶段清除交叉编译及浏览器验收环境，避免误启动长时间夹具；完成或失败后恢复调用者环境和工作目录。
- 模拟构建测试不会运行真实交叉编译，不清理用户目录。
- 运行器审计：`logs/test-runner-YYYY-MM-DD.jsonl`，包含阶段、结果与脱敏输出。

## Go

```bash
go vet ./...
go test ./...
go test -run '^TestConfigEditor' -count=1
go test -run 'TestOfficial|TestRequestProfile|TestHarnessProtocol' -count=1
```

`*_test.go` 直接使用 `package main` 私有实现，不搬进普通子目录。Go 在 `go build .` 时会排除这些文件，标准测试命令仍运行完整测试，不需要额外加载器。

测试进程使用临时工作目录，配置、凭据和上游响应均为合成数据，避免污染真实文件。
跨编译后的测试程序若在另一台机器运行，应提供同一提交的源码目录，并用 `WORKBUDDY_TEST_SOURCE_ROOT` 指定其绝对路径；这样即使使用 `-trimpath`，示例配置等文件测试也能正确定位，不必跳过这些测试。
出站兼容测试覆盖三协议 × 流式/聚合、gzip、显式参数保留、热加载及请求身份。
`rate_resolution_test.go` 覆盖统一倍率决策、官方与实测冲突、真实价格变动与重复刷新、促销时间边界、国际站别名、多来源优先级、日志截断及不可读、缓存重启恢复与写入失败恢复、旧账本升级，以及三协议 × 流式/聚合的成功和不完整响应。全部使用临时文件与本地合成上游，不消耗真实模型额度。
`identity_request_test.go` 验证文本替换仅由 `identity` 控制，新装/升级及热加载保持有效；旧 `promptCompatibility` 字段只兼容读取并告警，不执行规则，不影响用户/工具内容，也不会放宽其他未知字段的校验。本地 HTTP 验证三个入口 × 两种出站模式 × 流式/聚合 × 四种字面/通配开关场景，共 48 组，覆盖后续 system/assistant 文本块、模型字段及 user/tool 内容保护。
`identity_wildcard_test.go` 验证单个 `(.*)`、非空首尾、偏移零、非贪婪、单行/256 字符上限、Unicode、无正则展开、非法规则忽略且不回退默认、优先级、映射自身停用、热加载及日志不泄露正文；并验证旧硬编码规则只在配置且开启时执行，普通模板不会再改 user/tool，15 条默认规则（含默认通配）的新装/旧版升级保留自定义值及关闭状态、可幂等重启。真实上游渠道校验尚未验证。
`identity_restore_regression_test.go` 覆盖明确且唯一的身份声明、否定/普通提及、角色顺序与声明冲突，URL/域名/路径的任意三段切分、三协议 × 流式/聚合的正文及推理文本保护，关闭时 SSE 字节不变、热加载请求快照和审计落盘。新装/升级自动写入 `responseRestore:false`、已有设置保留及重复启动幂等性由 `identity_config_file_test.go` 验证。
`stream_test.go` 覆盖空工具身份字段、参数片段和扩展字段保护，以及交错工具分片与多段推理文本经 Chat 请求回传；`messages_test.go` 验证 thinking 开始事件包含字符串 `signature`，并保持签名增量与结束事件顺序。测试使用本地合成上游，实际服务验证范围见出站兼容说明。
可选历史采集离线重放见[出站兼容说明](../docs/outbound-compatibility.md#验证与边界)，默认不会读取私有材料。

## 网页

```bash
node --check web/app.js
node --test tests/web/app.test.cjs
```

`tests/web/app.test.cjs` 加载真实 `web/app.js`，使用合成 DOM / fetch 响应，不需要浏览器依赖、不调用生产接口。审计写入 `logs/webui-frontend-tests-dev-YYYY-MM-DD.jsonl`。
配置页测试同时覆盖 JSON 高亮的键、字符串、数字、布尔值、null、HTML 转义和编辑内容不变。

可选真实浏览器回归需要 Python Playwright 和可用 Chromium，不纳入默认测试入口：

```bash
python tests/web/polling.browser.py
python tests/web/polling.browser.py --browser "本机 Chromium 兼容浏览器的可执行文件路径"
```

该测试只加载仓库中的网页文件，将所有网络请求拦截为合成响应，验证成功/错误提示、页面位移、账号启停、表格局部更新、倍率详情保留及图表复用；不访问生产或上游，验收记录写入 `logs/polling-flicker-*.jsonl`。

## 构建脚本

```powershell
.\tests\build\build_script.test.ps1
```

使用模拟 Go 命令验证九目标矩阵、平台/架构筛选、已知文件清理、失败处理和环境恢复。临时材料在 `.codex-tmp/`，审计在 `logs/build-tests-YYYY-MM-DD.jsonl`。此测试不能替代真实交叉编译。

## 浏览器手动验收

默认 Go 测试不会运行浏览器夹具。明确开启后，它只启动本地 `127.0.0.1:18316` 管理页面，使用合成账号和管理 Key，不调用上游：

```powershell
$env:WORKBUDDY_BROWSER_FIXTURE = '1'
$env:WORKBUDDY_BROWSER_DURATION = '90s'
go test -run '^TestPolicyBrowserFixture$' -count=1 -timeout 3m -v
Remove-Item Env:WORKBUDDY_BROWSER_FIXTURE, Env:WORKBUDDY_BROWSER_DURATION
```

应让夹具时长小于测试超时。不要使用生产地址、真实 Key 或真实凭据替换该夹具。

## 目录与命名

见 [docs/project-layout.md](../docs/project-layout.md)。不要把一次性实验或可能含环境信息的报告加入公开测试目录。
