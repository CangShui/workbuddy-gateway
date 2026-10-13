# 目录与命名约定

本次整理只改变文件组织及测试入口，不改变运行时工作目录、配置格式、网关行为或二进制构建入口。

## 目录职责

```text
workbuddy-gateway/
├── README.md                    # 简短入口，链接到完整文档
├── *.go                         # package main 的生产源码
├── *_test.go                    # 同包 Go 单元测试，不进入 go build
├── go.mod / go.sum              # Go 依赖
├── build.ps1                    # 保留原有一键构建入口
├── config.example.json          # 用户配置与 Release 打包示例
├── web/                         # 仅保留 go:embed 使用的网页运行资源
│   ├── index.html
│   ├── app.js
│   └── style.css
├── docs/
│   ├── user-guide.md            # 完整使用手册
│   ├── contributors.md          # 贡献者署名
│   ├── project-layout.md        # 本文
│   └── local/                  # 本地历史报告，不入库
│       ├── reviews/
│       ├── deployments/
│       ├── releases/
│       ├── diagnostics/
│       └── issues/
├── tests/
│   ├── README.md
│   ├── build/build_script.test.ps1
│   └── web/app.test.cjs
├── scripts/test.ps1              # 统一测试入口
├── archive/
│   ├── README.md
│   └── local/                   # 与网关无关的本地附件，不入库
├── dist/                        # 本地构建输出，不入库
│   └── local/                   # 整理时保留的原有根目录二进制
├── logs/                        # 运行、构建与测试审计，不入库
├── .codex-tmp/                  # 原有本地隔离实验，保留并忽略
└── .playwright-mcp/              # 原有浏览器验收缓存，保留并忽略
```

`config.json`、账号 JSON、状态文件和模型缓存按原有运行约定保留。整理不会把运行日志或凭据移到文档目录，也不会删除旧产物。

## Go 测试为什么没有移动到 tests/go

当前 Go 测试使用 `package main`，直接验证未导出的函数、类型和全局状态。Go 按目录划分包，把这些文件直接移到子目录会成为另一个包，无法访问这些私有成员。

因此保留标准 Go 同包测试约定：

- 生产构建仍使用 `go build .`，Go 自动排除 `_test.go` 文件。
- 测试仍使用 `go test ./...`，无需 overlay、自定义加载器或测试代码副本。
- 测试命名统一为 `snake_case_test.go`，如 `runtime_config_test.go`、`model_account_test.go`。
- 浏览器验收夹具保留为 `web_policy_browser_test.go`，默认跳过；明确设置环境变量后才运行。

如果以后要彻底分离 Go 源码与测试目录，应先按公开 API 拆分包，再改写对应集成测试；不应仅为目录美观而导出所有内部实现。

## 文件命名

| 类型 | 规则 | 示例 |
|---|---|---|
| Go 测试 | `snake_case_test.go` | `price_probe_control_test.go` |
| 独立脚本测试 | `名称.test.扩展名` | `app.test.cjs`、`build_script.test.ps1` |
| 公共文档 | 小写 `kebab-case.md` | `user-guide.md` |
| 有日期的本地报告 | `YYYY-MM-DD-主题-report.md` | `2026-10-06-pr-11-code-review-report.md` |
| 版本历史材料 | `v版本-主题.md` | `v1.13.13-release-notes.md` |
| 构建产物 | 保留构建脚本的系统/架构后缀 | `workbuddy-gateway-linux-arm64` |

生产 Go 文件名暂不改动，避免无必要的源码路径变更。移动历史文件时保留正文，不重写当时的实验事实或日志路径。

## 本地材料与发布边界

- `docs/local/`、`archive/local/`、工具缓存、`dist/` 和 `logs/` 均被忽略，不能因本次整理而误入 Git。
- 原根目录无关的 `index.js` 归档为 `archive/local/agent-loop-plugin.js`，不再与网页入口混淆；没有改动其内容。
- 原根目录的 `workbuddy-gateway.exe` 移到 `dist/local/workbuddy-gateway.exe`，没有把它冒充成当前版本的新构建。
- 一次性实验脚本继续留在原缓存目录，避免破坏历史工具引用；未来正式维护的开发脚本统一放到 `scripts/`。
- 公共文档和测试不会加入 Release 运行包；`config.example.json` 仍留在根目录，供既有构建脚本打包。

## 验证

请在项目根目录执行：

```powershell
.\scripts\test.ps1
.\build.ps1 -Only linux -Architecture amd64 -OutputDir dist/layout-check
```

测试命令、网页验收条件和日志路径详见 [tests/README.md](../tests/README.md)。
