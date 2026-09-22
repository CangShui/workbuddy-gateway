# WorkBuddy Local Gateway

<img width="917" height="754" alt="image" src="https://github.com/user-attachments/assets/7dcfc461-1357-4991-9565-279047687898" />


基于腾讯 **CodeBuddy** 协议开发的**纯 Go、零 CGO 依赖、跨平台单二进制**本地 AI 代理网关。无 Web UI，全部通过命令行（CLI）完成登录、凭据续期与服务控制。

**同时支持两个上游站点**（同一套 `/v2/plugin/*` 协议，凭据按站点隔离，账号池可混挂轮询）：

| 站点 | 上游 | 登录方式 | 登录命令 |
|---|---|---|---|
| 国内站 | `copilot.tencent.com` / `www.codebuddy.cn` | 微信 / 企业微信扫码 | `login` |
| 国际站 | `www.workbuddy.ai` | 浏览器内登录（邮箱 / 验证码 / SSO） | `login -intl` |

---

## 目录

- [核心特性](#核心特性)
- [命令总览](#命令总览)
- [serve](#serve)
- [login](#login)
- [status](#status)
- [refresh](#refresh)
- [monitor](#monitor)
- [probe](#probe)
- [reset](#reset)
- [version / help](#version--help)
- [多账号池](#多账号池)
- [模型列表与倍率](#模型列表与倍率)
- [客户端接入](#客户端接入)
- [各平台部署](#各平台部署)
- [安全提示](#安全提示)
- [从源码构建](#从源码构建)

---

## 核心特性

- **国内 / 国际双站反代**：两个站点走同一套协议，凭据通过 `edition` 字段区分，刷新与对话自动路由到各自上游。
- **模型完全透传**：客户端传什么 `model` 就原样中继到上游，无白名单限制。`/v1/models` 仅用于客户端自动补全，不影响实际转发。
- **模型列表双来源合并**：实时接口 + npm 静态目录，按 ID 去重、接口优先；失败用本地缓存，两边都失败且无缓存时该站点本轮不展示模型（不影响调用）。
- **模型倍率与价格探测**：促销生效时展示 `credits` × factor；促销过期或接口无有效倍率时由余额未耗尽的同站点账号实测（启动即探测、重置后立即探测、每模型 12 小时一轮）。
- **多账号池 + 轮询负载均衡**：`-auth` 逗号分隔或 `-auth-dir` 目录，请求按 round-robin 分发；国内站与国际站账号可混挂。
- **模型级隔离**：`6004` 只冷却触发它的账号 + 模型，`14018` 只阻断该账号的当前收费模型，不再因为一个模型拖垮整个账号。
- **免费站点优先**：同一模型若「一个站点免费、另一个站点收费」，优先使用免费站点账号直至其受限；两个站点都收费（仅倍率不同）时不做倾斜，正常轮询。
- **免费/收费学习**：按「账号 + 模型」从响应 `usage.credit` 学习；`credit=0` 且样本足够（`total_tokens ≥ 100`）才判定免费，避免小样本误判。
- **国内站每日自动签到**：服务启动、凭据热加载时立即补签，之后每天 `UTC+8 09:00` 自动签到；国际站跳过。
- **凭据热加载（免重启）**：默认每 5 秒扫描凭据来源，新增 / 更新 / 删除凭据免重启生效。
- **授权失效自动禁用**：401/403 / `invalid token` / 登录过期时禁止调度、删除凭据文件并写入失效标记，重新 `login` 后自动恢复。
- **后台自动续期**：每 5 分钟检查 Token，距过期不足 15 分钟自动刷新并写回凭据文件。
- **流式分片规范化**：把上游每个分片携带的 `finish_reason:""` 归一化为 `null`，避免 Anthropic 翻译层误判 `stop_reason` 导致工具不执行。
- **工具调用序列自愈**：出站前按 `tool_call_id` 修复并行调用中夹入 message 的历史结构，合并 Responses API 拆散的并行调用，并删除无配对调用、孤儿或重复结果，避免国际站返回 `11148 tool_call_sequence_broken`。
- **OpenAI 兼容协议**：`/v1/chat/completions`（SSE 流式 + 非流式聚合）、`/v1/responses`（Responses API）、`/v1/models`、`/health`。

---

## 命令总览

```text
workbuddy-gateway [command] [options]

命令:
  serve     启动本地网关（默认命令，不带子命令时等同 serve）
  login     登录并获取 / 更新凭据
  status    查看账号池状态
  refresh   手动刷新所有账号访问令牌
  monitor   前台实时监控：账号表格 + 模型统计附表 + 最近日志
  probe     主动探测账号对指定模型的免费 / 收费属性（需 serve 运行中）
  reset     清空除登录凭据外的全部本地数据，并重新拉取模型与倍率
  version   查看版本信息
  help      查看帮助
```

全局选项（对所有命令可用）：

| 选项 | 默认 | 说明 |
|---|---|---|
| `-addr <ip>` | `127.0.0.1` | 网关监听地址 |
| `-port <port>` | `8317` | 网关监听端口 |
| `-auth <path>` | 自动发现 | 凭据文件路径，支持逗号分隔多个 |
| `-auth-dir <dir>` | 空 | 凭据目录，自动加载目录内所有 `workbuddy*.json` |
| `-api-key <key>` | 空 | 设置后调用网关必须携带 `Authorization: Bearer <key>` |
| `-proxy <url>` | 空 | 上游请求代理，如 `http://127.0.0.1:7890`、`socks5://...` |
| `-verbose` | `false` | 输出详细调试日志 |
| `-intl` | `false` | 仅 `login` 生效：登录国际站 |
| `-reload-interval <sec>` | `5` | 凭据热加载扫描间隔，`0` 关闭 |
| `-models-refresh <min>` | `60` | 模型目录刷新间隔，`0` 关闭 |

### JSON 调试日志

工作目录中的 `config.json` 控制结构化调试日志，默认关闭。修改后需要重启网关进程：

```json
{
  "debug": {
    "enabled": true
  }
}
```

开启后，网关把单行 JSON 写入 `logs/debug-YYYY-MM-DD.jsonl`；普通运行日志仍写入原来的 `logs/gateway-YYYY-MM-DD.log`，两者互不替代。可复制 `config.example.json` 作为起点。

### 模型黑白名单

`config.json` 的 `models` 段可按模型名启用黑白名单（大小写与首尾空白不敏感）：

```json
{
  "models": {
    "blocklist": ["deepseek-v4-pro"],
    "allowlist": []
  }
}
```

| 字段 | 说明 |
|---|---|
| `blocklist` | 黑名单，命中即禁用 |
| `allowlist` | 白名单，**非空时**只放行列表内模型，其余一律禁用 |

规则：

- 黑名单优先：命中黑名单直接禁用，即使同时出现在白名单里。
- 两个列表都为空或省略时不做任何限制（默认行为不变）。
- 被禁用的模型会从 `/v1/models`、`/health` 的 `model_count` 和 `monitor` 的模型统计附表中**直接隐藏**。
- 请求被禁用模型时返回 `403` 与中文提示，**不会消耗任何上游账号额度**：

```json
{
  "error": {
    "message": "模型 deepseek-v4-pro 已被网关禁用（命中黑名单），请联系管理员调整 config.json",
    "type": "model_disabled",
    "code": 403
  }
}
```

`serve` 启动横幅会打印当前名单状态，例如 `模型黑白名单: 已启用 (黑名单 1 个 / 白名单 0 个...)`。

每条 JSON 调试日志都包含时间、级别、稳定事件名、`trace_id`、`request_id`、服务/实例/版本、路由、方法、模型、账号、流式标记和累计耗时，并记录客户端地址、代理头、协议、TLS、Content-Type、Content-Length、deadline 等请求元数据。客户端传入的 `X-Trace-ID` 会优先复用并透传到上游。

请求体只记录以下安全摘要，不记录正文：

- `declared_body_bytes`、`actual_body_bytes`、`body_read_ms`
- `body_sha256_prefix`（SHA-256 前 12 位）
- `json_valid`、`json_decode_ms`
- `body_limit_bytes`、`body_limit_exceeded`（当前未设置请求体限制，因此分别为 `0`、`false`）
- `read_error_type`、脱敏截断后的 `read_error`

调试日志不会记录 `Authorization`、Cookie、API Key、Access Token、Refresh Token 或完整请求体。只记录是否提供 Authorization，以及凭据的不可逆短哈希 `api_key_fingerprint`。

---

## serve

启动本地网关，默认命令。

```bash
# 默认监听 127.0.0.1:8317，自动加载当前目录下所有 workbuddy*.json
workbuddy-gateway serve

# 自定义端口与监听地址
workbuddy-gateway serve -port 9000 -addr 0.0.0.0

# 显式指定多个凭据文件（逗号分隔，轮询）
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# 目录模式：加载目录内所有 workbuddy*.json
workbuddy-gateway serve -auth-dir ./auths

# 上游走代理 + 开启客户端鉴权 + 详细日志
workbuddy-gateway serve -proxy http://127.0.0.1:7890 -api-key sk-xxx -verbose

# 关闭凭据热加载
workbuddy-gateway serve -reload-interval 0

# 关闭模型目录自动刷新
workbuddy-gateway serve -models-refresh 0
```

启动后提供的端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions`、`/chat/completions` | Chat Completions，支持 SSE 流式与非流式 |
| POST | `/v1/responses`、`/responses` | OpenAI Responses API |
| GET | `/v1/models`、`/models` | 模型列表，响应头 `X-Model-Source` 标注来源 |
| GET | `/health`、`/ping` | 健康检查，返回 `version`、`model_count`、`model_source` |
| POST | `/admin/probe` | 供 `probe` 命令调用，**仅接受回环来源** |
| GET | `/` | 简单文本说明 |

后台任务（`serve` 启动后自动运行）：

| 任务 | 周期 | 说明 |
|---|---|---|
| Token 续期检查 | 5 分钟 | 距过期不足 15 分钟自动刷新 |
| 额度扫描 | 5 分钟 | 每凭据 10 秒超时，超时保留旧值；`剩余=0` 标记付费耗尽 |
| 模型目录刷新 | 60 分钟 | 实时接口 + npm 目录，合并去重后写缓存 |
| 模型价格探测 | 30 分钟检查 / 每模型 12 小时一轮 | 单轮最多 5 个，仅探测需要确认的模型 |
| 每日签到 | 每天 `UTC+8 09:00` | 仅国内站 |
| 状态快照 | 3 秒 | 写 `workbuddy-status.json` 供 `monitor` 读取 |
| 凭据热加载 | 5 秒 | 扫描凭据新增 / 更新 / 删除 |

**上游超时策略**

网关不再对上游请求设置「整条流的总超时」，避免长时间但持续有输出的流被中途掐断（会丢失 usage、返回残缺内容，并被上游代理判为故障触发 503）：

| 阶段 | 策略 |
|---|---|
| 连接与响应头 | `ResponseHeaderTimeout` 默认 300 秒。上游对 3MB+ 大请求排队+预处理可能接近 1 分钟（实测 2.3MB 请求曾需 50.8s 才回响应头），因此默认放宽到 5 分钟兜底 |
| 流式响应体 | 空闲读超时默认 120 秒：持续有数据就永不超时，只有 120 秒无任何新数据才判定卡死并中断 |
| 控制类短请求 | 令牌刷新 / 额度查询 / 模型目录等 60 秒超时 |
| 服务端写响应 | 不设总时长上限（原 300 秒），长流不会被服务端截断 |

两个上游超时可在工作目录 `config.json` 的 `upstream` 段覆盖（单位秒，省略或非正数则用默认值）：

```json
{
  "upstream": {
    "headerTimeoutSeconds": 300,
    "idleTimeoutSeconds": 120
  }
}
```

启动横幅会打印生效值，便于确认。流被中断时不会伪造 `[DONE]`（chat）或 `response.completed`（Responses），而是下发明确的 `upstream_stream_interrupted` / `response.failed` 错误事件，避免下游把残缺输出当成完整结果。

---

## login

登录并保存凭据。国内站输出终端 ASCII 二维码；国际站在浏览器内完成。

```bash
# 国内站（微信 / 企业微信扫码）
workbuddy-gateway login

# 保存到指定文件（多账号推荐）
workbuddy-gateway login -auth workbuddy2.json

# 国际站（浏览器内完成，邮箱 / 验证码 / SSO）
workbuddy-gateway login -intl
workbuddy-gateway login -intl -auth workbuddy-intl.json
```

说明：

- 默认保存到 `workbuddy.json`；`-auth` 可指定其他路径。
- 国际站凭据写入 `edition: "intl"`，与国内站凭据可混挂在同一账号池。
- 重新登录会覆盖原凭据并自动清除该账号的失效标记，无需重启服务（热加载会生效）。

---

## status

查看账号池状态，包含站点、冷却、额度与 Token 过期时间。

```bash
workbuddy-gateway status
```

输出示例：

```text
================== WorkBuddy 账号池状态 ==================
账号总数: 2

--- 账号 #1 ---
凭据文件:     workbuddy.json
站点:         国内站 (copilot.tencent.com)
用户昵称:     user-a
用户 UID:     uid-xxx
企业 ID:      (个人账号)
认证域名:     www.codebuddy.cn
冷却状态:     可用
Token 状态:   有效
过期时间:     2026-09-22 12:32:07 (剩余 119h30m0s)
```

---

## refresh

立即刷新所有账号的 Access Token（正常情况下由后台每 5 分钟自动检查，无需手动执行）。

```bash
workbuddy-gateway refresh
```

- 成功 / 失败 / 跳过（授权失效）会分别统计。
- 刷新失败若属于授权类错误，会禁用该账号并删除凭据文件。

---

## monitor

前台实时监控，周期刷新展示「账号表格 + 模型统计附表 + 最近日志」，`Ctrl+C` 退出。

```bash
# 必须在 serve 的工作目录执行（读取 workbuddy-status.json）
cd /opt/workbuddy-gateway
workbuddy-gateway monitor

# 附加展示 systemd 服务最近日志（Linux）
workbuddy-gateway monitor -journal workbuddy-gateway

# 附加展示指定日志文件
workbuddy-gateway monitor -logfile /var/log/workbuddy-gateway.log

# 调整刷新间隔与日志行数
workbuddy-gateway monitor -interval 2 -lines 20
```

| 选项 | 默认 | 说明 |
|---|---|---|
| `-interval <sec>` | `3` | 状态刷新间隔 |
| `-journal <svc>` | 空 | 同时展示 `journalctl -u <svc>` 最近日志 |
| `-logfile <path>` | 空 | 同时展示指定日志文件末尾内容 |
| `-lines <n>` | `15` | 每次展示的日志行数 |

**账号表格**

```text
账号池: 共 2 个 | 可用 1 | 冷却 0 | 付费耗尽 1 | 过期 0 | 失效 0
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| 序号 | 凭据文件       | 账号       | 站点   | 状态       | Token 有效期        | 总额度   | 已用     | 剩余   | 套餐       | 免费模型   | 模型冷却   |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| 1    | workbuddy.json | user-a     | 国内站 | 可用       | 2026-09-22 12:32:07 | 2300     | 1200     | 1100   | pro        | 1          | 0          |
| 2    | workbuddy2.json| user-b     | 国际站 | 付费耗尽   | 2027-09-05 01:57:00 | 1100     | 1100     | 0      | Pro试用    | 0          | 0          |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
```

状态取值：`可用`、`冷却`、`付费耗尽`、`已过期`、`失效`。

套餐取值：`Pro试用`（上游 `ProTrialStatus=1`）、`pro`（上游 `IsPaidUser=true`）、`免费`（其余，含识别不出）。

**模型统计附表**

```text
模型统计 (来源 live-api@2026-09-17 14:57):
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
| 模型                       | 国内倍率         | 国际倍率         | 可用账号 | 请求     | 平均首字(5h)  | 平均总耗时(5h)  | 总Token(M) |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
| hy3                        | 0.00x            | 0.00x            | 8        | 3        | 1.9s          | 2.3s            | 0.12M      |
| deepseek-v4.1-flash        | 0.03x            | 0.00x            | 5        | 12       | 820ms         | 3.4s            | 1.75M      |
| hy4-preview                | 0.00x            | 收费(倍率未知)   | 8        | 4        | 1.3s          | 4.1s            | 0.00M      |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
```

| 列 | 含义 |
|---|---|
| 模型 | 模型 ID |
| 国内倍率 | 国内站生效倍率（`credits` × 促销 factor）；免费显示 `0.00x`，促销过期或接口无有效倍率时显示 `-`，实测确认收费显示 `收费(倍率未知)` |
| 国际倍率 | 国际站同上 |
| 可用账号 | 当前可调度该模型的账号数（已计入账号冷却、模型冷却、模型额度阻断） |
| 请求 | 客户端请求次数 |
| 平均首字(5h) | 最近 5 小时滚动窗口内的平均首字响应时间（TTFT），按小时分桶、自动淘汰过期样本 |
| 平均总耗时(5h) | 最近 5 小时滚动窗口内的平均总耗时 |
| 总Token(M) | 进程启动后累计的上游 usage token，单位百万；优先 `usage.total_tokens`，没有则用 `prompt_tokens + completion_tokens`；上游未返回 usage 的请求不估算 |

---

## probe

免费 / 收费属性按「账号（含站点）+ 模型」学习，只有该账号真正请求过该模型才会写入账本。默认调度优先使用有余额账号，**余额耗尽的账号几乎不会被选中，也就学不到属性**。`probe` 用于主动补课。

> 注意：额度耗尽的账号会被上游整体拒绝（`14018 Credits exhausted`），此时连免费模型也会失败。要验证某模型是否免费，请使用**额度未耗尽**的账号。

```bash
# 探测全部账号，每个账号取模型目录前 5 个模型
workbuddy-gateway probe

# 只探测指定账号
workbuddy-gateway probe -auth workbuddy4.json

# 指定模型
workbuddy-gateway probe -auth workbuddy4.json -models hy3,deepseek-v4.1-flash

# 指定数量上限（默认 5，上限 50）
workbuddy-gateway probe -auth workbuddy4.json -limit 8
```

| 选项 | 默认 | 说明 |
|---|---|---|
| `-auth <path>` | 全部账号 | 只探测指定凭据（文件名或路径均可） |
| `-models <m1,m2>` | 目录前几个 | 指定要探测的模型 |
| `-limit <n>` | `5` | 未指定 `-models` 时探测的模型数量，上限 50 |

输出示例：

```text
正在请求 http://127.0.0.1:8317/admin/probe（账号=workbuddy4.json，模型=hy3）...

账号                   站点   模型     结果     credit  tokens  说明
workbuddy4.json        intl   hy3      paid     0.42    820     usage.credit=0.42，收费

汇总: paid=1
```

结果状态：

| 状态 | 含义 |
|---|---|
| `free` | `usage.credit=0` 且 `total_tokens ≥ 100`，已学习为免费 |
| `paid` | `usage.credit > 0`，已学习为收费 |
| `unknown` | 未返回 `credit`，或 `credit=0` 但样本过小 |
| `quota` | `14018` 额度耗尽，记为该账号该模型收费并阻断该模型 |
| `rate_limited` | `6004` 模型级限流，只冷却该模型 |
| `auth_failed` | 授权失效（probe 不会自动禁用账号） |
| `skipped` | 账号失效或无凭据 |
| `error` | 网络 / 协议错误 |

> 原理：`probe` 作为客户端调用运行中服务的 `/admin/probe`。账本保存在 `serve` 进程内存中，独立进程直接写状态文件会被服务快照覆盖，因此探测必须由运行中的服务执行。该接口仅接受回环来源；服务启用 `-api-key` 时同样需要鉴权。

---

## reset

清空**除登录凭据以外**的全部本地数据，并重新拉取模型与倍率。

```bash
workbuddy-gateway reset
```

清理范围：

- `workbuddy-status.json`（账号与模型状态快照）
- `wb-models-cache.json`（模型目录、倍率、价格探测结论）
- `*.disabled` / `*.json.disabled`（授权失效标记）
- `logs/`（运行日志）

保留：`workbuddy*.json` 登录凭据。

清理后会立即重新拉取模型目录与倍率。账号账本同时存在于 `serve` 进程内存中，若服务正在运行，请重启使其同步归零：

```bash
systemctl restart workbuddy-gateway
```

---

## version / help

```bash
workbuddy-gateway version    # 输出 WorkBuddy Local Gateway vX.Y.Z
workbuddy-gateway help       # 输出完整帮助
workbuddy-gateway -v         # 同 version
workbuddy-gateway -h         # 同 help
```

---

## 多账号池

三种配置方式：

```bash
# 方式一（推荐）：自动发现
# 把多个凭据文件放进工作目录，无需任何参数
workbuddy-gateway serve

# 方式二：-auth 逗号分隔
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# 方式三：-auth-dir 目录
workbuddy-gateway serve -auth-dir ./auths
```

行为说明：

- **轮询**：请求按 round-robin 在可用账号间分发。
- **429 冷却**：`6004` 只冷却触发模型；无法归因到模型的 429 才进入账号级冷却，冷却到期自动恢复。
- **授权失效**：401/403 类错误禁用账号并删除凭据文件，同时写 `*.disabled` 标记；重新 `login` 后自动恢复。
- **额度耗尽**：`剩余=0` 标记「付费耗尽」，仍可服务已确认免费的模型。
- **热加载**：默认每 5 秒扫描，新增 / 更新 / 删除凭据免重启。
- **串行化**：同一账号请求严格排队，避免并发双发触发风控；不同账号可并行。

---

## 模型列表与倍率

**列表来源**：实时接口 `GET {Base}/v2/enterprises/personal/models` 与 npm 包静态目录，按模型 ID 去重、**接口优先**。

```text
两路都成功  → 合并去重
一路成功    → 使用成功那路
两路都失败  → 使用本地缓存 wb-models-cache.json
失败且无缓存→ 该站点本轮不展示模型（不影响模型调用）
```

**免费站点优先**：若某模型出现「一个站点免费、另一个站点收费」，调度优先使用免费站点的账号，直到该站点账号全部不可用（冷却 / 耗尽 / 失效）才回退到另一站点；若两个站点都免费或都收费（只是倍率不同），则不设优先，保持正常轮询。

**倍率**：

```text
1. 促销生效中：生效倍率 = credits × factor（factor=0 → 0.00x）
2. 促销已过期：接口 credits 不可信（上游常把促销价固化进 credits），
   探测出结果前显示 -，随后由实测决定
3. 模型不在接口目录中：同样交由实测决定
4. 无促销且 credits 有值：直接展示该倍率
```

**价格探测**：由「余额未耗尽」的同站点账号发一次最小请求实测。

```text
探测免费 → 展示 0.00x，并每 12 小时复测确认
探测收费 → 展示 收费(倍率未知)，直到接口重新给出未过期的 0.00x
14018 / 无 usage.credit / 样本过小 → 不覆盖，保持未知
```

探测调度：

| 时机 | 说明 |
|---|---|
| 服务启动 | 启动后约 20 秒执行首轮 |
| 首次 / 重置后 | 单轮最多 30 个，快速补齐结论 |
| 收敛后 | 单轮最多 5 个，每模型 12 小时最多一次 |
| 待探测未清空 | 用 2 分钟短间隔追赶，清空后回到 30 分钟 |
| 目录刷新成功 | 立即触发一轮 |
| 凭据变化 | 立即触发一轮（含「原本没有某站点账号、后来加入」的情况） |

仅探测被实际请求过、或接口明确需要确认的模型，避免无谓消耗额度。

`/v1/models` 响应头 `X-Model-Source` 与 `/health` 的 `model_source` 会标注目录来源。

---

## 客户端接入

网关启动后服务地址为 `http://127.0.0.1:8317/v1`。

curl：

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

DSH（`~/.dsh/settings.yaml`）：

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

---

## 各平台部署

### Windows

1. 从 [Releases](https://github.com/CangShui/workbuddy-gateway/releases) 下载 `workbuddy-gateway-windows-amd64.exe`。
2. 在 PowerShell / CMD 中进入文件所在目录：

   ```powershell
   .\workbuddy-gateway-windows-amd64.exe login
   .\workbuddy-gateway-windows-amd64.exe serve -port 8317
   ```

3. 开机自启：`Win+R` → `shell:startup`，把 exe 快捷方式放入启动文件夹，并在快捷方式“目标”后追加 `serve`。

### Linux

```bash
# x86_64
wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-amd64
sudo install -m 755 workbuddy-gateway-linux-amd64 /usr/local/bin/workbuddy-gateway

# ARM64
wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-arm64
sudo install -m 755 workbuddy-gateway-linux-arm64 /usr/local/bin/workbuddy-gateway

workbuddy-gateway login
workbuddy-gateway serve -addr 127.0.0.1 -port 8317
```

#### systemd 服务（推荐）

创建 `/etc/systemd/system/workbuddy-gateway.service`：

```ini
[Unit]
Description=WorkBuddy Local Gateway (CodeBuddy OpenAI-compatible proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/workbuddy-gateway
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317
Restart=on-failure
RestartSec=5
User=root
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=false

[Install]
WantedBy=multi-user.target
```

部署与启动：

```bash
sudo mkdir -p /opt/workbuddy-gateway
sudo cp workbuddy-gateway /opt/workbuddy-gateway/
sudo /opt/workbuddy-gateway/workbuddy-gateway login
sudo systemctl daemon-reload
sudo systemctl enable --now workbuddy-gateway
sudo systemctl status workbuddy-gateway
sudo journalctl -u workbuddy-gateway -f
```

> `WorkingDirectory` 决定自动发现的凭据目录。把多个凭据文件放进该目录即可组成账号池，新增 / 更新 / 删除会自动热加载。

常用运维：

```bash
sudo systemctl restart workbuddy-gateway
sudo systemctl stop workbuddy-gateway
sudo systemctl disable workbuddy-gateway
```

对外开放时（例如局域网其他设备）把 `-addr` 改为 `0.0.0.0`，并**务必**设置 `-api-key`：

```ini
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317 -api-key sk-changeme
```

### macOS

1. 下载 `workbuddy-gateway-darwin-arm64`（Apple Silicon）或 `workbuddy-gateway-darwin-amd64`（Intel）。
2. 移除隔离属性：

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

---

## 安全提示

- `workbuddy*.json` 包含真实访问凭据（Access Token / Refresh Token），**严禁提交到 Git 或公开分享**；本仓库 `.gitignore` 已排除。
- 网关默认只监听 `127.0.0.1`。需要局域网 / 公网访问时改用 `-addr 0.0.0.0` 并配合 `-api-key`，或置于反向代理之后。
- `/admin/probe` 仅接受回环来源调用。
- 不再需要某账号授权时，删除对应凭据文件并在 CodeBuddy 控制台撤销授权。

---

## 从源码构建

需要 Go 1.20+：

```bash
git clone https://github.com/CangShui/workbuddy-gateway.git
cd workbuddy-gateway

go vet ./...
go test ./...

# 当前平台
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o workbuddy-gateway .

# 交叉编译示例
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/workbuddy-gateway-linux-amd64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/workbuddy-gateway-windows-amd64.exe .
```

---

## 免责声明

本项目仅用于个人学习与技术研究。腾讯 CodeBuddy（含国内站与国际站 workbuddy.ai）的接口协议与风控策略可能随时变化；请遵守腾讯服务条款，自行承担使用风险。本仓库不包含任何官方未公开的密钥或凭据。
