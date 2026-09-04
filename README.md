# WorkBuddy Local Gateway
<img width="1734" height="924" alt="image" src="https://github.com/user-attachments/assets/d25431ed-0240-464a-a65d-70aaf3909477" />

基于腾讯 **CodeBuddy / 混元（Hunyuan）** 协议开发的**纯 Go、零 CGO 依赖、跨平台单二进制**本地 AI 代理网关。无 Web UI，仅通过命令行（CLI）完成扫码登录、凭据续期与服务控制。

## 核心特性

- **模型完全透传**：客户端（OpenAI SDK、Cursor、Claude Code、DSH 等）传什么 `model`（如 `hy4-preview`、`hy3-preview-agent`、`hy3`、`deepseek-v4-pro`、`glm-5.2`…），网关原样透传至腾讯上游，无白名单限制。
- **纯 CLI 控制**：终端内嵌 ASCII 二维码，微信 / 企业微信扫码登录；`status` / `refresh` / `serve` 子命令完成全部管理。
- **多账号池 + 轮询负载均衡**：支持同时挂载多个 CodeBuddy 账号（`-auth` 逗号分隔或 `-auth-dir` 目录），请求按轮询（round-robin）均匀分配到各账号，保持多账号额度使用一致。
- **429 频率限制自动冷却**：任一账号触发上游频率限制（HTTP 429 / code 6004）时，自动解析消息中的重置时间（如 `将在 2026-09-04 07:48:15 UTC+8 重置`），将该账号屏蔽至重置时间；冷却期间自动改用其他账号代偿，冷却到期自动恢复。
- **授权失效自动禁用**：账号授权过期、被撤销或令牌刷新失败（HTTP 401/403 / invalid token / 登录已过期）时，自动将该账号**禁止调度并删除凭据文件**，同时写入持久化失效标记；`status` / 启动日志会明确提示该账号失效原因与重新登录命令，重新 `login` 后自动恢复调度。
- **后台自动续期**：运行期间每 5 分钟检查所有账号 Token，距过期不足 15 分钟自动刷新并持久化回各自凭据文件。
- **OpenAI 兼容协议**：`POST /v1/chat/completions`（SSE 流式 + 非流式聚合）、`GET /v1/models`、`GET /health`。
- **深度思考透传规则**：仅当客户端显式请求 `reasoning_effort` 时转发；绝不强制注入，避免触发上游内容安全策略。
- **反审查净化**：自动改写 Claude Code 等框架被上游逐字拉黑的固定 Prompt 语句。
- **单账号串行化**：同一账号请求自动排队，避免并发双发触发上游风控；不同账号之间可并行。

## 快速上手

### 1. 扫码登录

首次使用或凭据失效时执行（需微信 / 企业微信扫码）：

```bash
# Windows
.\workbuddy-gateway-windows-amd64.exe login

# Linux / macOS
./workbuddy-gateway-linux-amd64 login
./workbuddy-gateway-darwin-arm64 login
```

终端将打印 ASCII 二维码与浏览器直达链接，扫码后凭据自动保存到当前目录 `workbuddy.json`（请勿提交到代码仓库）。

> 提示：建议在 **CodeBuddy 控制台（网页端）** 扫码登录获取更高权限评级的 Token；若使用 `login` 命令扫码，账号渠道可能受限（`azp=invite`）。

### 2. 查看凭据状态

```bash
workbuddy-gateway status
```

### 3. 启动本地网关

```bash
# 默认监听 127.0.0.1:8317
workbuddy-gateway serve

# 自定义端口 / 地址 / 详细日志
workbuddy-gateway serve -port 8317 -verbose

# 通过出口代理转发（可选，降低上游风控概率）
workbuddy-gateway serve -proxy http://127.0.0.1:7890

# 开启客户端鉴权
workbuddy-gateway serve -api-key sk-localsecret
```

## 多账号池与 429 自动冷却

### 配置多个账号

网关支持同时挂载多个 CodeBuddy 账号，请求按**轮询（round-robin）**方式均匀分发，保持各账号额度消耗一致。三种配置方式：

```bash
# 方式一（推荐）：自动发现 — 把多个凭据文件放进工作目录即可，无需任何参数
# 网关启动时自动加载当前目录下所有 workbuddy*.json（如 workbuddy.json、workbuddy2.json…）
cd /opt/workbuddy-gateway
workbuddy-gateway serve -addr 0.0.0.0 -port 8317
# 启动日志会显示: 已就绪账号池: 2 个账号 (有效 2, 失效 0)

# 方式二：-auth 逗号分隔多个凭据文件
workbuddy-gateway serve -auth workbuddy.json,workbuddy-2.json,workbuddy-3.json

# 方式三：-auth-dir 指定凭据目录（自动加载目录下所有 workbuddy*.json）
mkdir -p auths
workbuddy-gateway login -auth auths/workbuddy-1.json   # 依次为每个账号扫码登录
workbuddy-gateway login -auth auths/workbuddy-2.json
workbuddy-gateway serve -auth-dir ./auths
```

### 429 频率限制自动冷却

当某个账号触发上游频率限制（HTTP 429，消息形如 `您的使用量已超出频率限制，将在 2026-09-04 07:48:15 UTC+8 重置`）时，网关会：

1. **自动解析消息中的重置时间**，立即将该账号屏蔽（冷却）至该时间点；
2. **自动改用下一个可用账号重试**当前请求（代偿），无需客户端干预；
3. 冷却期间该账号不参与轮询，**冷却到期后自动恢复**；
4. 若消息中无法解析重置时间，默认冷却 60 秒后重试；
5. 当所有账号均处于冷却状态时，返回 HTTP 429 并附上最早解封时间。

```bash
# 查看各账号状态（含冷却状态与解封时间）
workbuddy-gateway status

# 手动刷新所有账号令牌
workbuddy-gateway refresh
```

### 授权失效自动禁用

当账号出现以下任一情况时，网关会**自动禁用该账号调度并删除其凭据文件**：

- 令牌刷新失败且返回授权类错误（HTTP 401/403、`invalid token`、`unauthorized`、`登录已过期` 等）；
- 上游请求返回 401/403（token 被撤销或已过期）；
- 凭据缺少 RefreshToken 且已无法刷新。

处理流程：

1. 将该账号标记为 **❌ 授权失效**，立即移出轮询调度；
2. **删除对应的凭据文件**（如 `workbuddy.json`），并写入持久化失效标记（`workbuddy.json.disabled`）；
3. 控制台（`status` / 启动日志 / 运行日志）明确显示失效原因，并给出重新登录命令；
4. 失效期间其余账号正常代偿；**重新执行 login 后自动清除失效标记并恢复调度**。

```bash
# 查看失效账号与原因
workbuddy-gateway status
# 输出示例：
# --- 账号 #1 ---
# 凭据文件:     workbuddy-2.json
# 用户昵称:     tester
# 用户 UID:     uid-xxx
# 账号状态:     ❌ 授权失效（禁止调度）
# 失效原因:     令牌刷新失败 (HTTP 401): invalid token
# 处理建议:     ⚠️ 凭据文件已删除，请重新执行: workbuddy-gateway login -auth workbuddy-2.json

# 失效账号重新登录后自动恢复
workbuddy-gateway login -auth workbuddy-2.json
workbuddy-gateway status   # 该账号恢复为 ✅ 可用
```

### 与单账号模式的兼容性

- 不传 `-auth` / `-auth-dir` 时，自动发现当前工作目录下所有 `workbuddy*.json`；目录中只有一个凭据文件时行为与旧版完全一致；
- 只有一个账号时，请求始终使用该账号，429 冷却逻辑同样生效（冷却期间请求将返回 429 提示）；
- 账号之间使用独立的串行锁：同一账号请求严格排队，不同账号可并行，兼顾风控与吞吐。

## 前台实时监控 (monitor)

网关以 systemd / 后台方式运行时，可用 **`monitor` 命令在前台实时查看所有账号的最新状态与最近日志**（Ctrl+C 退出）：

```bash
# 基本用法：每 3 秒刷新展示账号池状态（可用/冷却/失效 + Token 有效期）
cd /opt/workbuddy-gateway        # 必须与 serve 同一工作目录（读取 workbuddy-status.json）
workbuddy-gateway monitor

# 自定义刷新间隔（秒）
workbuddy-gateway monitor -interval 2

# 同时展示 systemd 服务最近日志（Linux）
workbuddy-gateway monitor -journal workbuddy-gateway

# 或展示指定日志文件最近内容
workbuddy-gateway monitor -logfile /var/log/workbuddy-gateway.log

# 控制每次展示的日志行数
workbuddy-gateway monitor -journal workbuddy-gateway -lines 8
```

展示内容（实时刷新）：

```text
================ WorkBuddy 实时监控 ================
按 Ctrl+C 退出 | 状态文件: workbuddy-status.json
---------------------------------------------------------------
🕐 更新时间: 2026-09-04 09:35:12
📊 账号池: 共 2 个 | ✅ 可用 1 | 🔒 冷却 1 | ❌ 失效 0
  #1 workbuddy.json (Abandon)
     ✅ 可用 | Token 有效期至: 09-10 20:43
  #2 workbuddy2.json (啊水)
     🔒 冷却中 至 09-05 01:57 (剩余 16h)
     原因: 您的使用量已超出频率限制...
📜 最近日志 (journalctl -u workbuddy-gateway):
  9月 04 09:34:31 ... [Cooldown] 账号 workbuddy2.json 触发频率限制...
---------------------------------------------------------------
```

> 原理：`serve` 后台每 3 秒（及状态变化时）将账号池实时状态原子写入同目录 `workbuddy-status.json`，`monitor` 前台读取该文件并周期刷新展示；日志通过 `journalctl` 或日志文件补充展示。

## 客户端接入

网关启动后服务地址为 `http://127.0.0.1:8317/v1`。

```bash
curl -N -s http://127.0.0.1:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"hy4-preview","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

Python OpenAI SDK：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8317/v1", api_key="none")
resp = client.chat.completions.create(
    model="hy4-preview",
    messages=[{"role": "user", "content": "写一个快速排序"}],
)
print(resp.choices[0].message.content)
```

DSH (`~/.dsh/settings.yaml`)：

```yaml
llm-pi-ai:
  providers:
    workbuddy-local:
      baseURL: http://127.0.0.1:8317/v1
      apiKeyEnv: LOCAL_API_KEY   # 任意字符串即可
      api: openai-completions
      models:
        - id: hy4-preview
          contextWindow: 1000000
          maxTokens: 128000
```

## 各平台使用方法

### Windows

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载 `workbuddy-gateway-windows-amd64.exe`。
2. 在 PowerShell / CMD 中进入文件所在目录：
   ```powershell
   .\workbuddy-gateway-windows-amd64.exe login
   .\workbuddy-gateway-windows-amd64.exe serve -port 8317
   ```
3. 如需开机自启：`Win+R` → `shell:startup`，将 exe 的快捷方式放入启动文件夹即可（命令行加 `serve` 参数需通过快捷方式"目标"追加）。

### Linux (amd64 / arm64)

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载对应架构二进制，赋执行权限并放入 PATH：
   ```bash
   # x86_64
   wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-amd64
   sudo install -m 755 workbuddy-gateway-linux-amd64 /usr/local/bin/workbuddy-gateway
   # ARM64 (树莓派 / 飞腾 / Apple silicon 云主机等)
   wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-arm64
   sudo install -m 755 workbuddy-gateway-linux-arm64 /usr/local/bin/workbuddy-gateway
   ```
2. 登录并启动：
   ```bash
   workbuddy-gateway login      # 终端二维码扫码（可用 tmux 保持）
   workbuddy-gateway serve -addr 127.0.0.1 -port 8317
   ```

#### Linux 注册为 systemd 服务（推荐，开机自启 + 崩溃自动拉起）

创建 `/etc/systemd/system/workbuddy-gateway.service`：

```ini
[Unit]
Description=WorkBuddy Local Gateway (CodeBuddy/Hunyuan OpenAI-compatible proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# 二进制与 workbuddy.json 所在目录；请按实际部署路径修改
WorkingDirectory=/opt/workbuddy-gateway
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 127.0.0.1 -port 8317
Restart=on-failure
RestartSec=5
User=root
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=false

[Install]
WantedBy=multi-user.target
```

部署文件：

```bash
sudo mkdir -p /opt/workbuddy-gateway
sudo cp workbuddy-gateway /opt/workbuddy-gateway/      # 对应架构的二进制
# 首次登录（会生成 workbuddy.json）
sudo /opt/workbuddy-gateway/workbuddy-gateway login
# 登录第二个账号（可选）：生成 workbuddy2.json
sudo /opt/workbuddy-gateway/workbuddy-gateway login -auth /opt/workbuddy-gateway/workbuddy2.json
```

> 💡 **多账号自动发现**：服务通过 `WorkingDirectory` 固定在 `/opt/workbuddy-gateway`，
> 无需修改 ExecStart——把多个凭据文件（`workbuddy.json`、`workbuddy2.json`…）放进该目录，
> 重启服务即自动加载全部账号组成轮询池。确认方式：
> ```bash
> sudo journalctl -u workbuddy-gateway | grep 账号池
> # 输出示例: 已就绪账号池: 2 个账号 (有效 2, 失效 0)
> ```

启用并启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now workbuddy-gateway
sudo systemctl status workbuddy-gateway     # 查看状态
sudo journalctl -u workbuddy-gateway -f     # 查看实时日志
```

常用运维命令：

```bash
sudo systemctl restart workbuddy-gateway    # 重启（如更换凭据后）
sudo systemctl stop workbuddy-gateway       # 停止
sudo systemctl disable workbuddy-gateway    # 取消开机自启
```

如需对外开放（例如给局域网其他设备使用），将 `-addr` 改为 `0.0.0.0`，**并务必**配合 `-api-key` 设置访问密钥：

```bash
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317 -api-key sk-changeme
```

### macOS (Apple Silicon / Intel)

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载 `workbuddy-gateway-darwin-arm64`（M 系列）或 `workbuddy-gateway-darwin-amd64`（Intel）。
2. 首次运行需移除隔离属性：
   ```bash
   chmod +x workbuddy-gateway-darwin-arm64
   xattr -d com.apple.quarantine workbuddy-gateway-darwin-arm64 2>/dev/null || true
   ```
3. 登录与启动：
   ```bash
   ./workbuddy-gateway-darwin-arm64 login
   ./workbuddy-gateway-darwin-arm64 serve
   ```
4. 开机自启（launchd）：创建 `~/Library/LaunchAgents/com.workbuddy.gateway.plist`：
   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key><string>com.workbuddy.gateway</string>
     <key>ProgramArguments</key>
     <array>
       <string>/path/to/workbuddy-gateway-darwin-arm64</string>
       <string>serve</string>
       <string>-port</string><string>8317</string>
     </array>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>WorkingDirectory</key><string>/path/to/workbuddy-gateway-dir</string>
   </dict>
   </plist>
   ```
   ```bash
   launchctl load ~/Library/LaunchAgents/com.workbuddy.gateway.plist
   ```

## 安全提示

- `workbuddy.json` 包含真实 CodeBuddy 访问凭据（Access Token / Refresh Token），**严禁提交到 Git 仓库或公开分享**；本仓库 `.gitignore` 已将其排除。
- 网关默认只监听 `127.0.0.1`。需要局域网 / 公网访问时请改用 `-addr 0.0.0.0` 并配合 `-api-key` 鉴权，或置于反向代理（如 nginx）之后。
- 若不再需要某账号的授权，请删除对应 `workbuddy.json` 并在 CodeBuddy 控制台撤销应用授权。

## 命令行速查

```
命令:  serve | login | status | refresh | monitor | version | help
选项:  -addr <ip> · -port <port> · -auth <path> · -auth-dir <dir> · -api-key <key> · -proxy <url> · -verbose
monitor: -interval <sec> · -journal <svc> · -logfile <path> · -lines <n>
```

## 从源码构建

需要 Go 1.20+：

```bash
git clone https://github.com/CangShui/workbuddy-gateway.git
cd workbuddy-gateway
CGO_ENABLED=0 go build -ldflags="-s -w" -o workbuddy-gateway .
```

## 免责声明

本项目仅用于个人学习与技术研究。腾讯 CodeBuddy 的接口协议与风控策略可能随时变化；请遵守腾讯服务条款，自行承担使用风险。本仓库不包含任何官方未公开的密钥或凭据。
