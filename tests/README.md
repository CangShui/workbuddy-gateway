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
出站兼容测试覆盖三协议 × 流式/聚合、gzip、显式参数保留、热加载及请求身份。
可选历史采集离线重放见[出站兼容说明](../docs/outbound-compatibility.md#验证与边界)，默认不会读取私有材料。

## 网页

```bash
node --check web/app.js
node --test tests/web/app.test.cjs
```

`tests/web/app.test.cjs` 加载真实 `web/app.js`，使用合成 DOM / fetch 响应，不需要浏览器依赖、不调用生产接口。审计写入 `logs/webui-frontend-tests-dev-YYYY-MM-DD.jsonl`。

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
