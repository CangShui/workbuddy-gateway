# WorkBuddy Local Gateway

纯 Go、零 CGO 依赖的跨平台单二进制网关，支持国内站与国际站、多账号调度、OpenAI / Anthropic 协议及可选网页控制台。

## 快速开始

需要 `go.mod` 声明的 Go 1.26.5 或更新工具链。在项目根目录执行：

```bash
go build -o workbuddy-gateway .
./workbuddy-gateway login
./workbuddy-gateway serve -webui
```

Windows 可使用 `.\workbuddy-gateway.exe`。管理端口默认 `8316`，模型 API 默认 `8317`。

- 首次使用可复制 `config.example.json` 为 `config.json`，并按需要设置管理 Key 和 API 鉴权。
- **后台价格探测默认关闭**，只有显式传入 `-allow-price-probes` 才开启；主动生成请求可能消耗积分。
- 网页配置页支持风险确认后的完整 JSON 编辑；监听端口修改需重启服务。
- `config.json` 和账号凭据含敏感信息，已由 Git 忽略，不要公开分享。
- 多 Harness 可使用 `upstream.requestProfile: "official"` 开启统一出站兼容；旧配置省略该项保持原行为。见[出站兼容说明](docs/outbound-compatibility.md)。

## 文档

| 内容 | 位置 |
|---|---|
| 完整使用手册、配置、客户端接入与部署 | [docs/user-guide.md](docs/user-guide.md) |
| 目录职责与命名约定 | [docs/project-layout.md](docs/project-layout.md) |
| 贡献者及方案署名 | [docs/contributors.md](docs/contributors.md) |
| 测试入口与说明 | [tests/README.md](tests/README.md) |
| 多 Harness 出站对齐、协议参数与兼容边界 | [docs/outbound-compatibility.md](docs/outbound-compatibility.md) |
| DSH 历史回放、GUI 环境句与 11128 兼容说明 | [docs/11128-dsh-history.md](docs/11128-dsh-history.md) |

## 构建与测试

```bash
go vet ./...
go test ./...
```

PowerShell：

```powershell
.\scripts\test.ps1                 # Go、网页和构建脚本回归
.\build.ps1                       # 校验并构建全部 9 个平台/架构目标
.\build.ps1 -Only linux -Architecture amd64
```

Go 单元测试保持与 `package main` 源码同目录，以保留私有函数测试和标准 `go test ./...` 行为；它们不进入生产构建。独立网页与构建脚本测试集中在 `tests/`。完整文档集中在 `docs/`，根目录保留构建入口与配置示例。

本项目仅用于个人学习与技术研究，请遵守上游服务条款。完整安全提示与免责声明见[使用手册](docs/user-guide.md#安全提示)。
