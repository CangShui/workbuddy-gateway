# WorkBuddy Local Gateway

<img width="917" height="754" alt="image" src="https://github.com/user-attachments/assets/7dcfc461-1357-4991-9565-279047687898" />


基于腾讯 **CodeBuddy** 协议开发的**纯 Go、零 CGO 依赖、跨平台单二进制**本地 AI 代理网关。默认无 Web UI，全部通过命令行（CLI）完成登录、凭据续期与服务控制；可选开启只读管理台（`-webui`，见下文）。

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
- [webui（只读管理台）](#webui只读管理台)
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
- **后台自动续期**：每 5 分钟检查 Token，距过期不足 15 分钟自动刷新并写回凭据文件；另可按 `-keepalive-hours` 在固定时刻主动刷新全部账号（默认每天 22:00），避免长期空闲账号静默失效。
- **流式分片规范化**：把上游每个分片携带的 `finish_reason:""` 归一化为 `null`，避免 Anthropic 翻译层误判 `stop_reason` 导致工具不执行。
- **工具调用序列自愈**：出站前按 `tool_call_id` 修复并行调用中夹入 message 的历史结构，合并 Responses API 拆散的并行调用，并删除无配对调用、孤儿或重复结果，避免国际站返回 `11148 tool_call_sequence_broken`。
- **OpenAI 兼容协议**：`/v1/chat/completions`（SSE 流式 + 非流式聚合）、`/v1/responses`（Responses API）、`/v1/models`、`/health`。
- **Anthropic 协议原生入口**：`/v1/messages`（SSE 流式 + 非流式聚合）与 `/v1/messages/count_tokens`，Claude Code 等 Anthropic 客户端免外部翻译层直连，鉴权兼容 `x-api-key` 头。

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
| `-port <port>` | `8317` | API 监听端口；显式传入时覆盖 `config.json` 的 `gateway.apiPort` |
| `-auth <path>` | 自动发现 | 凭据文件路径，支持逗号分隔多个 |
| `-auth-dir <dir>` | 空 | 凭据目录，自动加载目录内所有 `workbuddy*.json` |
| `-proxy <url>` | 空 | 上游请求代理，如 `http://127.0.0.1:7890`、`socks5://...` |
| `-verbose` | `false` | 输出详细调试日志 |
| `-intl` | `false` | 仅 `login` 生效：登录国际站 |
| `-reload-interval <sec>` | `5` | 凭据热加载扫描间隔，`0` 关闭 |
| `-models-refresh <min>` | `60` | 模型目录刷新间隔，`0` 关闭 |
| `-disable-price-probes` | `false` | 禁止后台自动价格探测，避免自动发起模型生成请求；不影响客户端请求及显式 `probe` 命令 |
| `-webui` | `false` | 启用独立网页控制台（默认 `http://127.0.0.1:8316/ui/`）；管理 Key 与模型 API 鉴权由 `config.json` 配置 |

### 配置网页控制台与模型 API 鉴权

工作目录 `config.json` 的 `gateway` 段控制监听端口、网页控制台管理 Key 和模型 API 鉴权，全部为明文配置（`config.json` 已被 Git 忽略，请自行限制文件权限）：

```json
{
  "gateway": {
    "apiPort": 8317,
    "webPort": 8316,
    "adminKey": "",
    "apiKeyEnabled": false,
    "apiKey": ""
  }
}
```

| 配置 | 默认 | 说明 |
|---|---|---|
| `apiPort` | `8317` | 模型 API 监听端口，省略或 `0` 使用默认值；也可用 `-port` 临时覆盖 |
| `webPort` | `8316` | 网页控制台监听端口，省略或 `0` 使用默认值；仅在显式 `-webui` 时监听 |
| `adminKey` | `""` | 网页控制台管理 Key，有两种设置方式：**① 页面一键生成**——为空时在服务器本机（或 SSH 本地端口转发）打开网页，生成 32 字符随机 Key 写入本文件，并在弹窗中**仅展示一次**；**② 手动编辑本文件**——直接把本字段改成任意随机字符串（建议 32 位）后重启网关。设置完成后可从其他设备用该 Key 登录 |
| `apiKeyEnabled` | `false` | 模型 API 是否校验 Key。关闭时不带 Key 或任意 Key 均可调用模型；开启时必须匹配 `apiKey` |
| `apiKey` | `""` | 模型 API Key。开启校验时必须非空；关闭时保留原值，重新开启即可继续使用 |

网页控制台也可在「访问设置」页切换 `apiKeyEnabled` 并设置 `apiKey`，保存后立即生效并写回本文件；端口修改需要重启。管理 Key 与模型 API Key **相互独立**：关闭模型 API 鉴权不会关闭管理鉴权，模型 API Key 也不能登录网页控制台。为避免他人抢先生成，**页面一键生成**管理 Key 只允许本机访问；**手动编辑 `config.json` 设置 `gateway.adminKey`** 不受此限制，效果与一键生成完全相同。

> **升级顺序（重要）**：新版本能读取不含 `gateway` 段的旧配置，但**旧版本二进制遇到含 `gateway` 段的新配置会启动失败**（`json: unknown field "gateway"`）。请**先替换二进制、再添加 `gateway` 配置**。

### JSON 调试日志

工作目录中的 `config.json` 控制结构化调试日志，默认关闭。修改后需要重启网关进程：

```json
{
  "debug": {
    "enabled": true,
    "logSystemPrompt": false
  }
}
```

开启后，网关把单行 JSON 写入 `logs/debug-YYYY-MM-DD.jsonl`；普通运行日志仍写入原来的 `logs/gateway-YYYY-MM-DD.log`，两者互不替代。可复制 `config.example.json` 作为起点。

`debug.logSystemPrompt` 默认 `false`。置为 `true` **且** `debug.enabled` 同时为 `true` 时，`system_prompt_policy_applied` 事件会额外记录提示词正文：

- `final_system_body`：最终发往上游的首条 system 正文（含 `fallback` / `force` 组合后的结果），排查人设、风格类问题最实用；
- `client_system_body`：覆盖发生前客户端提供的 system 原正文（仅在 `fallback` 非空、真的覆盖了客户端 system 时出现，便于对比）；
- 内容块数组场景记录各 text 块拼接后的纯文本（以换行分隔）。

无论该开关是否开启，事件始终记录不含正文的审计字段，便于判断"到底哪一版提示词发出去了"：

- `final_system_chars` / `client_system_chars`：最终 / 覆盖前正文的字符数（后者仅在真的覆盖了客户端自带 system 时出现）；
- `final_system_kind`：最终 system 内容的形态（`字符串` / `内容块数组` / `空内容` / `未支持的内容类型`）。内容不是字符串或 text 块数组时取不到正文，`final_system_chars` 会是 `0`，此时靠本字段区分"没有 system"与"取不到正文"；
- `final_system_sha256_prefix` / `client_system_sha256_prefix`：对应正文的 SHA-256 前 12 位十六进制指纹，可用于跨请求、跨版本比对提示词是否漂移；
- `prompt_mode`（生效模式：`覆盖` / `透传` / `保底`——`fallback` 非空一律为 `覆盖`；`fallback` 为空时按客户端是否自带 system 记 `透传` 或 `保底`）/ `fallback_source`（文本来源：`配置覆盖` / `配置保底` / `内置保底` / `客户端透传`）/ `override_applied`（是否真的丢弃了客户端自带 system）：本次命中的模式、来源与分支；
- `forced_applied` / `forced_content_kind`（`未修改` / `字符串` / `内容块数组` / `空内容` / `未支持的内容类型，已跳过强制后置`）/ `forced_chars`：`force` 是否真的追加到了末尾、以什么形态追加、追加了多少字符。

普通运行日志里同一事件另有一行中文摘要，字段与上面一一对应，例如：

```
[系统提示词规则] traceId=… requestId=2 阶段=上游请求序列化前 生效模式=覆盖 文本来源=配置覆盖 强制全局已应用=false 强制位置=后置(紧贴用户消息) 强制内容类型=未修改 强制字符数=0 结果=覆盖客户端system为配置fallback且未记录提示词正文
```

`结果=` 只有四种取值，与 `文本来源=` 一一对应：`覆盖客户端system为配置fallback` / `客户端未提供system，已注入配置fallback` / `客户端未提供system，已使用内置兜底提示词` / `保留客户端原有system`。

> 提示词正文可能很长（例如上万字的人设）：开启 `logSystemPrompt` 后单条事件可达数十 KB。只有在确需正文时才开启，排查完建议改回 `false`；`debug.enabled=false` 时即使把 `logSystemPrompt` 设为 `true` 也不会记录正文，启动横幅会明确提示"未生效"。

普通运行日志（`logs/gateway-*.log` 与终端输出）在任何配置下都**不记录提示词正文**，该开关只影响 JSON 调试日志；正文仍会经过统一的敏感值脱敏。

### 配置系统提示词

工作目录 `config.json` 的 `systemPrompt` 段只有两个选项（修改后需重启 `serve`）；**语义完全由 `fallback` 是否为空决定，没有模式开关**：

```json
{
  "systemPrompt": {
    "fallback": "",
    "force": ""
  }
}
```

| 客户端 system | `fallback` 非空 | `fallback` 空/全空白 |
|---|---|---|
| 有 | **`fallback`**（覆盖客户端，客户端原文丢弃） | 客户端原 system（透传） |
| 无 | **`fallback`**（保底注入） | 内置保底 `You are a helpful assistant.` |

| 配置 | 空值行为 | 非空行为 |
|---|---|---|
| `fallback` | 覆盖关闭：客户端自带 system 原样透传；客户端没给时才用内置保底 | 覆盖开启：无论客户端是否自带 system，发往上游的首条 system 都是这段文本（客户端没给时即"保底"） |
| `force` | 不启用 | **实验功能**：在三个入口的首条 system 内容**末尾追加**配置文本（后置），使其成为位置最靠后、紧贴用户消息的指令；未发生覆盖时原 system 内容仍完整保留 |

要点：

- 空串与全空白视为未配置（`"  "` 等同 `""`），不会触发覆盖。
- 「首条消息必须是 system」的兜底注入始终存在（上游 code 11128 硬校验依赖它）；它只决定客户端**没有** system 时用什么文本，与是否覆盖无关。
- `force` 与覆盖正交：`fallback` 非空且 `force` 非空时，发往上游的是 `fallback\n\nforce`（字符串形态）或「fallback 文本块 + force 文本块」（内容块数组形态）。
- 例：`{"fallback":"你是一个助手。","force":"请用中文回答。"}` → 无论客户端是否自带 system，上游收到的都是 `你是一个助手。\n\n请用中文回答。`；把 `fallback` 改成 `""` 后，客户端自带 system 时上游收到的是 `<客户端原 system>\n\n请用中文回答。`。
- 配置值仅支持 JSON 字符串。不会改写客户端的 user、assistant、tool 消息，也不改变账号选择。普通日志和 JSON 调试日志只记录规则分支、位置与字符数，**不记录提示词正文**（需要正文时显式开启 `debug.logSystemPrompt`）。

> **实验功能风险自负：** `force` 后置于 system 末尾（紧贴用户消息），因此比客户端自带指令更"新近"，冲突时通常更有影响力——这正是人设类、风格类指令希望达到的效果，但也可能与客户端的 system 指令冲突、改变模型行为、增加 token 消耗，或触发上游内容/参数校验。`fallback` 非空时会**丢弃客户端自带 system**：客户端原本靠自己的 system 声明的角色、工具约定、输出格式都会失效，请只在自己的客户端上使用。需要回退时将 `fallback` 清空、`force` 改回 `""` 并重启；建议先在测试会话验证，切勿在配置中写入密码、密钥或个人隐私。`config.json` 已被 Git 忽略，不要将真实配置提交到仓库。

`/v1/messages/count_tokens` 按**本次真正会发往上游的 system**做本地近似估算（`fallback` 非空时计 `fallback`，为空时计客户端自带 system、缺省时计内置兜底，再叠加 `force`），不调用上游、不消耗账号额度。估算规则：CJK 每字 1 token、ASCII 每 4 字符 1 token，所以纯中文会偏高约三成、以 ASCII 为主的长文本误差约 5%；最终消耗以模型返回的 usage 为准。日志与调试事件会同时给出 `生效模式` / `文本来源`，与 `[系统提示词规则]` 的口径一致。

### 套餐识别与官网一致性

从 v1.13.9 起，账号套餐不再仅凭 `ProTrialStatus=1` 判定为“Pro试用”。每轮额度刷新会额外调用官网的只读通用资源接口，根据有效状态、权益起止时间及当前订阅信息确认套餐：

- 查询不限定 `PackageCodes`，支持分页；官网新增套餐编码不会被固定名单漏掉。
- 名称按站点区分：国内站四档规范显示为 **体验版 / 标准版 / 高级版 / 旗舰版**；国际站保留 **免费 / pro / Pro试用**。国内标准版不会误标为国际站 pro。
- 新套餐优先显示接口返回的 `PackageName`，历史套餐也不会强行归入四档。新编码没有名称时显示“未识别订阅”，不猜成 Free 或 Pro。
- 赠送积分包、加量包不当成订阅；额度用尽但权益未到期的套餐仍保留套餐身份。
- 已过期、退款、尚未生效的权益不当成当前套餐。试用标记仍为 `1`，但已无有效试用且只有 Free 权益时，显示“免费”。
- 查询失败或遇到无法解释的字段/子产品时，已有核验结果标 `*`（待刷新），无核验结果显示“待确认”。额度查询成功时仍正常更新额度，不因套餐识别失败丢弃额度。
- 快照增加 `planCheckedAt`、`planStale` 字段。旧版本未核验的套餐标签不会作为可信结果沿用；新版本启动后会重新校验。

兼容边界：同一产品和资源字段结构下新增套餐通常可直接展示官方名称；若官网改变产品编码、接口、状态枚举或权益规则，仍可能需要更新程序。这不是对未来任意接口变更的保证。此过程只查询会员/额度信息，不领取试用、不购买/续费，也不请求模型。

### 登录凭据自动续期（保活）

登录凭据分两种令牌：**访问令牌**用于请求模型和查询额度；**刷新令牌**用于换取新的访问令牌。刷新令牌本身也有有效期，一旦它到期或被上游判定失效，就必须重新 `login`，程序无法绕过。

从 v1.13.11 起，网关同时使用两条触发路径续期：

| 触发方式 | 时机 | 说明 |
|---|---|---|
| 临近过期续期 | 后台每 5 分钟检查，访问令牌剩余不足 15 分钟时刷新 | 保证请求不会用到过期令牌 |
| **主动定时续期** | `-keepalive-hours` 指定的本地时刻（默认 `22`，即每天 22:00） | 不等访问令牌临近过期就刷新全部账号，避免长期空闲账号静默失效 |

```bash
# 默认每天 22:00 主动续期
workbuddy-gateway serve

# 每天 10:00 和 22:00 各一次
workbuddy-gateway serve -keepalive-hours 10,22

# 关闭主动续期，只保留临近过期时的按需刷新
workbuddy-gateway serve -keepalive-hours ""
```

实现要点：

- **只调刷新接口**，不请求模型、不消耗额度、不产生对话记录。
- **串行 + 节流**：账号之间间隔 1 秒逐个刷新，避免批量并发刷新形成机器特征。
- **启动补跑**：距上次成功续期超过 20 小时的账号，在服务启动 90 秒后补做一轮，避免重启错过时刻。
- **失败不轻易删号**：单次失败只累计计数并保留原凭据；连续失败 3 次才判定登录态失效并停止调度。上游明确返回会话失效（如 `12153` / `invalid_grant`）时首次即告警，但仍保留凭据等待阈值。
- **记录续期元数据**：刷新成功后把 `refreshExpiresAt`、`lastRefreshTime` 写回凭据文件，`status` 与 `monitor` 会显示刷新令牌到期时间，并在剩余不足 7 天或已过期时给出重新登录提醒。
- **不掩盖问题**：刷新成功但访问令牌期限没有顺延时会单独告警，提示该账号可能已接近上游的登录态上限。

> **注意刷新令牌是独占的。** 每次续期上游通常会下发新的刷新令牌并让旧的失效。如果同一个账号的凭据还被其他工具（脚本、其他网关、桌面客户端）同时使用，双方会互相把对方的刷新令牌作废。请确保一个账号只由一处负责续期。`-keepalive-hours` 留空即可关闭本功能。

#### 凭据覆盖与删除前的校验

凭据文件是登录态的唯一副本，删掉就只能重新登录。因此「覆盖」和「删除」这两个不可逆动作之前都会先做只读校验。

**续期成功、准备覆盖旧凭据之前：**

| 检查 | 目的 | 不通过时 |
|---|---|---|
| 新旧凭据账号标识一致（访问令牌 `sub`） | 防止上游串号或响应错配，把凭据写到别的账号上 | 拒绝覆盖，保留旧凭据并告警 |
| 新旧凭据站点一致（`iss` realm） | 防止国内站/国际站凭据互相覆盖 | 同上 |
| 新访问令牌能取到账号数据 | 防止「接口返回 200 但实际不可用」的凭据覆盖掉还能用的旧凭据 | 同上 |

校验接口自身不可用时按通过处理，不会因为校验抖动而拒绝有效的新凭据。校验失败**不计入**「判定失效」的计数，因此不会把还能用的账号停掉。

**判定失效、准备删除凭据之前：**

1. 先停止调度并写入失效标记；
2. 再用只读接口确认该凭据确实取不到账号数据，**才**删除文件；
3. 凭据仍能取到数据 → 保留文件；
4. 无法判定（网络故障、上游 5xx）→ 保守保留文件。

凭据文件被保留时，失效标记在重启后依然生效，账号不会被重新调度。如果你重新 `login` 或手动续期了该凭据，凭据文件会比标记更新，网关会自动清除过期标记并恢复该账号。

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

### 按模型限制账号文件

`models.accounts` 是**模型专属**的凭据 JSON 文件黑白名单，不是全局账号名单；未配置的模型仍可使用原有账号池。与上面的 `models.allowlist` / `models.blocklist`（控制模型是否可调用）互不替代：

```json
{
  "models": {
    "blocklist": [],
    "allowlist": [],
    "accounts": {
      "deepseek-v4.1-flash": {
        "allowlist": ["intl-a.json", "intl-b.json"],
        "blocklist": ["intl-b.json"]
      },
      "hy3": {
        "blocklist": ["old-account.json"]
      }
    }
  }
}
```

- 同一模型内黑名单优先；账号白名单为空表示不限制，黑名单为空表示不排除。上述示例中 `deepseek-v4.1-flash` 最终只允许 `intl-a.json`，`hy3` 仅排除 `old-account.json`。
- 只接受凭据**文件名**（如 `intl-a.json`），不接受路径或通配符；同名凭据位于多个目录时会拒绝匹配，避免误用。模型名忽略大小写，文件名必须与实际凭据文件一致。
- 请求调度与失败换号都不会绕过账号名单；后台价格探测与本机 `/admin/probe` 也会跳过不允许的账号。若没有匹配的账号，请求返回 `403 model_account_disabled` 中文提示，且不会调用上游。
- 实时运行日志 `logs/gateway-YYYY-MM-DD.log` 在有账号被排除时记录汇总一行（模型、候选账号数、排除明细）；开启调试时 `logs/debug-YYYY-MM-DD.jsonl` 记录每个账号的 `model_account_policy_checked`。`monitor` 模型统计表的“可用账号”列已按名单过滤，只统计符合规则的账号。
- `config.json` 在服务启动时读取，修改后需重启网关；示例中的文件名均为占位值。没有配置 `models.accounts` 时原行为不变。

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

# 上游走代理 + 详细日志（客户端鉴权在 config.json 的 gateway 段配置）
workbuddy-gateway serve -proxy http://127.0.0.1:7890 -verbose

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
| POST | `/v1/messages` | Anthropic Messages API（Claude Code 直连，内部转 Chat 走同一条上游管线） |
| POST | `/v1/messages/count_tokens` | Anthropic 令牌计数（本地估算，按生效后的 system 计；CJK 1 token/字、ASCII 4 字符/token） |
| GET | `/v1/models`、`/models` | 模型列表，响应头 `X-Model-Source` 标注来源 |
| GET | `/health`、`/ping` | 健康检查，返回 `version`、`model_count`、`model_source` |
| POST | `/admin/probe` | 供 `probe` 命令调用，**仅接受回环来源** |
| GET | `/ui/` | 网页控制台静态页面（独立端口，仅 `-webui` 启用时存在） |
| GET | `/admin/api/*` | 网页控制台数据接口（独立端口，仅 `-webui` 启用时存在，需管理 Key） |
| GET | `/` | 简单文本说明 |

从 v1.13.15 起，Responses `function_call_output.output` 支持文本和图片内容块数组：
文本原样保留在 `tool` 结果中，`input_image` 图片提升为全部配对工具结果之后的
`user` 多模态消息，图片 URL 和数据不变。图片不再被序列化成 Base64 文本混入
上下文，避免截图会话异常消耗大量 token，最终触发 `11133 model_param_invalid`。
并行工具调用的结果仍连续排列，不会因插入图片而破坏配对。普通字符串和 JSON
工具结果保持原行为；网关不截断操作手册或会话历史。`11133` 本身是上游通用参数
错误码，不仅限于图片或上下文长度问题；排障仍需结合模型、TraceID 与日志。

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
    "idleTimeoutSeconds": 120,
    "transientRetries": 2
  }
}
```

**瞬时网络错误重试**

网关与上游 CDN 边缘节点之间的单条 TCP 连接可能被对端重置（`connection reset by peer`）、被关闭（`use of closed network connection`），或命中已被回收的 keep-alive 连接。这类错误属于瞬时故障，与请求体大小无关（实测 >5MB 请求 95% 成功，而 0.5MB 请求也会偶发失败）。

只有**请求头尚未写出**时，才能确认上游不可能处理本次 POST，网关才对瞬时连接错误重试。`Do` 返回 EOF、RST 或超时但请求头已写出时，上游可能已经收到并处理请求；即使尚未收到响应头，也不会自动重放，以免重复生成。

| 项 | 默认 | 说明 |
|---|---|---|
| 重试次数 | 2 | `upstream.transientRetries` 可覆盖；显式设为 `0` 可禁用 |
| 重试间隔 | 300ms | 给上游边缘节点留出恢复时间 |
| 重试范围 | 请求头未写出时的瞬时错误 | 包括建连阶段 EOF、RST、`broken pipe`、`connection refused` 等 |
| 不重试 | — | 请求头已写出的 EOF/RST/超时（执行结果不确定）、客户端取消、上游 HTTP 错误响应 |
| 连接处理 | 新连接 | 重试时设置 `Connection: close` 并 `CloseIdleConnections()`，避免复用坏连接 |

重试会在日志中留下明确记录：

```text
[网络重试] traceId=... requestId=389 账号=example.json 第 1/2 次重试，上一尝试请求头未写出，已重建连接
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
| 1    | workbuddy.json | user-a     | 国内站 | 可用       | 2026-09-22 12:32:07 | 2300     | 1200     | 1100   | pro        | 2          | 0          |
| 2    | workbuddy2.json| user-b     | 国际站 | 付费耗尽   | 2027-09-05 01:57:00 | 1100     | 1100     | 0      | Pro试用    | 2          | 0          |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
```

状态取值：`可用`、`冷却`、`付费耗尽`、`已过期`、`失效`。

“免费模型”列按账号所属站点，统计下方模型统计附表中对应倍率为 `0.00x` 的模型数量，不再统计该账号自己的实测免费记录。该数量仅用于展示，不代表该账号一定可调用这些模型；实测账本、`probe`、账号调度和计费判断仍保持原有逻辑。

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

## webui（网页控制台）

默认关闭。显式 `-webui` 后在 `gateway.webPort`（默认 `8316`）提供独立网页控制台，用于在浏览器查看账号池、模型统计、日志、`config.json` 与凭据文件（令牌脱敏），并在「访问设置」页切换模型 API Key 校验。它与模型 API 端口分离，未启用时两个端口都不监听。

```bash
# 只有显式 -webui 才开启网页控制台；模型 API 仍监听 8317
workbuddy-gateway serve -webui

# 方式一：本机或 SSH 本地端口转发打开页面，一键生成管理 Key
ssh -L 8316:127.0.0.1:8316 root@服务器
# 浏览器打开
# http://127.0.0.1:8316/ui/

# 方式二：不打开页面，直接在服务器上手动设置管理 Key（建议 32 位随机字符串）
python3 - <<'PY'
import json, secrets, pathlib
p = pathlib.Path("/opt/workbuddy-gateway/config.json")
cfg = json.loads(p.read_text())
cfg.setdefault("gateway", {})["adminKey"] = secrets.token_hex(16)  # 32 字符
p.write_text(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n")
print("已写入 gateway.adminKey，重启网关后生效")
PY
systemctl restart workbuddy-gateway
```

| 项 | 说明 |
|---|---|
| 入口 | `GET /ui/`（独立管理端口，静态外壳不含任何数据） |
| 数据接口 | `GET /admin/api/status`、`/admin/api/models`、`/admin/api/logs`、`/admin/api/config`、`/admin/api/credentials`，**一律需要 `Authorization: Bearer <adminKey>`** |
| 写入接口 | `POST /admin/api/setup`（仅本机首次生成管理 Key；也可改用下方“手动编辑配置文件”方式）、`POST /admin/api/settings`（切换模型 API 鉴权并写入 `config.json`） |
| 页面 | 总览（含国内站 / 国际站账号数量）、账号、模型、日志、配置（只读 · Key 已隐藏）、凭据（只读 · 脱敏）、访问设置 |
| 数据来源 | 复用 `serve` 周期写入的 `workbuddy-status.json`、`logs/` 日志与本地配置/凭据文件，不改变转发与调度逻辑 |
| 凭据展示 | 只显示站点、昵称、UID、认证域名与到期时间；令牌仅保留前 6 位（如 `ACCESS****`），不返回明文 |

- 未启用 `-webui` 时，管理端口不监听，`/ui` 与 `/admin/api/*` 也不注册路由。
- **管理 Key 的两种设置方式**（二选一即可）：
  1. **页面一键生成**：`adminKey` 为空时，在服务器本机、或用 SSH 本地端口转发（`ssh -L 8316:127.0.0.1:8316 用户@服务器`）后访问 `http://127.0.0.1:8316/ui/`，点击生成 32 字符随机 Key；Key 写入 `config.json` 并**仅在弹窗中展示一次**。远程（非本机）访问不能触发一键生成，避免他人抢先生成；但远程仍可用已设置好的 Key 登录。
  2. **手动编辑配置文件**：直接在服务器上编辑 `config.json`，把 `gateway.adminKey` 设为你自己的随机字符串（建议 32 位），保存后重启网关即可。此方式不需要本机访问，也不需要页面操作；登录时填写该字符串即可。
- `/admin/api/logs` 只允许读取 `logs/` 下的 `gateway-YYYY-MM-DD.log` 与 `debug-YYYY-MM-DD.jsonl`，拒绝任意路径与目录穿越；返回日志先做秘密值脱敏，且单次读取有约 1 MiB 的扫描预算，超长单行会被省略。
- 前端为原生 HTML/CSS/JS，通过 `go:embed` 嵌入二进制，无额外构建步骤与运行时依赖；管理 Key 只保存在当前标签页的 `sessionStorage`，验证成功后才保存。
- 状态快照缺失或损坏时接口返回 `503`，页面保留上次数据并提示，不会把读取失败显示成零账号。

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

> 原理：`probe` 作为客户端调用运行中服务的 `/admin/probe`。账本保存在 `serve` 进程内存中，独立进程直接写状态文件会被服务快照覆盖，因此探测必须由运行中的服务执行。该接口仅接受回环来源；模型 API 启用 Key 校验时同样需要携带 `gateway.apiKey`。

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

Claude Code（Anthropic 协议直连，免外部翻译层）：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export ANTHROPIC_API_KEY=你的模型APIKey   # config.json 未开启 apiKeyEnabled 时随意填
claude
```

> 网关在 `/v1/messages` 入口完成 Anthropic → Chat 双向转译：思维链回译为 `thinking` 内容块、
> `tool_calls` 回译为 `tool_use`、`finish_reason` 映射 `stop_reason`（`tool_calls→tool_use`、
> `length→max_tokens`），并复用账号池轮询、工具序列自愈与 WAF 脱敏管线。
> 模型名直接填网关 `/v1/models` 列表中的 ID。

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

对外开放时（例如局域网其他设备）把 `-addr` 改为 `0.0.0.0`，并在 `config.json` 中开启模型 API Key 校验：

```json
{"gateway":{"apiKeyEnabled":true,"apiKey":"sk-changeme"}}
```

```ini
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0
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
- 网关默认只监听 `127.0.0.1`。需要局域网 / 公网访问时改用 `-addr 0.0.0.0`，并在 `config.json` 中开启模型 API Key 校验（`gateway.apiKeyEnabled`），或置于反向代理之后。
- `config.json` 中的 `gateway.adminKey` 与 `gateway.apiKey` 按要求明文保存，请限制该文件权限；网页配置页只显示脱敏后的内容。
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
