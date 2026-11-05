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

- 首次启动会在工作目录生成完整的默认 `config.json`；旧配置自动补齐缺失的 `identity`、`concurrent` 配置，并补入默认的 `workspace root → workspace base`、`are paths the user → are paths that the user` 兼容替换，已有自定义值不覆盖。默认单账号模型并发 `4`，全局不限；修改 `concurrent.MaxAcc/MaxGlobal` 可热加载。
- v1.14.43 将旧 DSH 身份句、GUI 描述及固定模板改写统一写入 `identity.replacements`，并默认加入 `"You are(.*)released by xAI": "You are WorkBuddy"`；新装生成、旧版升级启动补齐缺失项，已有值及 `enabled:false` 保留。共 15 条默认规则；首位通配限制单行、动态部分最多 256 字符，不判断客户端品牌。规则、开关和目标修改均可热加载。
- **响应身份还原仍是实验功能，默认关闭**：`identity.responseRestore: false`。新装和升级启动会将缺失开关写为 `false`，不会自动开启。手动开启后，仅依据明确且唯一的客户端身份声明还原响应正文及推理文本，保留 URL、文件路径和工具参数；关闭时保持原响应转发行为。
- **后台价格探测默认关闭**，只有显式传入 `-allow-price-probes` 才开启；主动生成请求可能消耗积分。
- 网页配置页支持风险确认后的完整 JSON 编辑；API/Web 监听端口修改可热切换，已有连接与流继续完成。新端口被占用或配置无法应用时保留旧运行状态；显式 `-port` 仍优先于配置文件。
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
