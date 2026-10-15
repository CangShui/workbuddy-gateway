// 使用真实前端函数和合成响应，无浏览器/第三方依赖，不调用生产接口。
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const {randomUUID} = require("node:crypto");

const projectRoot = path.resolve(__dirname, "../..");
const testDate = new Date().toLocaleDateString("sv-SE", {timeZone: "Asia/Shanghai"});
const testTrace = "webui-tests-" + randomUUID();
const logPath = path.join(projectRoot, "logs", "webui-frontend-tests-dev-" + testDate + ".jsonl");
function audit(event, fields = {}) {
  fs.mkdirSync(path.dirname(logPath), {recursive: true});
  fs.appendFileSync(logPath, JSON.stringify({traceId: testTrace, event, ...fields}) + "\n");
}
function response(status, body) {
  return {status, ok: status >= 200 && status < 300, json: async () => body};
}
function fixture(fetchImpl = async () => response(200, {}), cookies = new Map()) {
  const elements = new Map(), session = new Map(), local = new Map(), alerts = [];
  const cookieWrites = [];
  function element(id) {
    if (!elements.has(id)) {
      const node = {innerHTML: "", textContent: "", className: "", value: "", checked: false, disabled: false,
            style: {}, querySelector() { return this; }, querySelectorAll() { return []; },
            addEventListener() {}, focus() {}, removeAttribute() {}, setAttribute() {}};
      node.classList = {
        add(name) { node.className += " " + name; },
        remove(name) { node.className = node.className.split(/\s+/).filter(v => v !== name).join(" "); },
        toggle(name, force) {
          const present = node.className.split(/\s+/).includes(name);
          const enabled = force === undefined ? !present : force;
          if (enabled && !present) this.add(name);
          if (!enabled && present) this.remove(name);
        },
        contains(name) { return node.className.split(/\s+/).includes(name); },
      };
      elements.set(id, node);
    }
    return elements.get(id);
  }
  const storage = map => ({getItem: k => map.get(k) || null, setItem: (k, v) => map.set(k, v), removeItem: k => map.delete(k)});
  const context = {
    document: {readyState: "loading", addEventListener() {}, getElementById: element, querySelectorAll: () => []},
    sessionStorage: storage(session), localStorage: storage(local),
    window: {alert: message => alerts.push(message), confirm: () => true, addEventListener() {}, location: {protocol: "http:", reload() {}}},
    TextEncoder, fetch: fetchImpl, setInterval: () => 1,
  };
  Object.defineProperty(context.document, "cookie", {
    configurable: true,
    get() { return [...cookies].map(([name, value]) => name + "=" + value).join("; "); },
    set(value) {
      cookieWrites.push(value);
      const pair = value.split(";")[0];
      const at = pair.indexOf("=");
      cookies.set(pair.slice(0, at), pair.slice(at + 1));
    },
  });
  let source = fs.readFileSync(path.join(projectRoot, "web", "app.js"), "utf8");
  source = source.replace(/\}\)\(\);\s*$/, `globalThis.review = {
    state, api, renderAccounts, renderModels, renderStatus, refreshActive,
    renderPoolHealth, renderAccountAlerts, loadAccountAlerts, lockDashboard,
    saveKey, saveAPISettings, initializeAdmin, discoverSetup,
    showConfigWarning, loadConfigEditor, saveConfigEditor, exitConfigEdit, setTab, lockDashboard,
    initListFilters, changeListFilter, reportRestoredListFilters,
    rateCell, rateKey, rateDetailHTML, ensureRateDetails, toggleRateDetail, restoreRateDetails
    , renderStats, loadStats, changeStatsFilter, statsLineChart, statsComboChart, statsDuration, statsNumber, statsCredit
    , openAddAccount, startAddAccount, pollAddAccount, closeAddAccount, accountRowActions
  };})();`);
  vm.createContext(context);
  vm.runInContext(source, context);
  return {review: context.review, element, session, local, alerts, window: context.window,
    document: context.document, cookies, cookieWrites};
}
function cells(html) {
  return [...html.matchAll(/<td(?: [^>]*)?>(.*?)<\/td>/g)].map(m => m[1]);
}

test("运行状态只展示健康度，无账号和过旧快照不能显示100%", () => {
  const f = fixture();
  f.review.renderPoolHealth({health: {percent: 100, state: "healthy"}});
  assert.equal(f.element("poolHealth").textContent, "100%");
  assert.equal(f.element("poolHealthBar").style.width, "100%");
  f.review.renderPoolHealth({health: {percent: 66.6, state: "degraded"}});
  assert.equal(f.element("poolHealth").textContent, "66.6%");
  assert.match(f.element("poolHealthStatus").textContent, /部分受限/);
  f.review.renderPoolHealth({health: {percent: null, state: "empty"}});
  assert.equal(f.element("poolHealth").textContent, "—");
  assert.match(f.element("poolHealthStatus").textContent, /未加载账号/);
  f.review.renderPoolHealth({stale: true, health: {percent: 100, state: "healthy"}});
  assert.equal(f.element("poolHealth").textContent, "—");
  assert.equal(f.element("poolHealthBar").style.width, "0%");
  const html = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(!html.includes('id="summaryCards"'));
  assert(!html.includes('id="siteSummary"'));
  assert(html.includes('id="accountsTable"'), "账号池详细状态不能删掉");
  audit("账号池健康度通过", {unknown_not_100: true, count_cards_removed: true});
});
test("总览最上面是运行状态（健康度与告警），用量与性能在其后", () => {
  const html = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  const start = html.indexOf('id="panel-overview"');
  const end = html.indexOf('id="panel-accounts"');
  assert(start > -1 && end > start, "总览面板必须存在");
  const overview = html.slice(start, end);
  const heading = overview.indexOf("overview-subheading");
  const health = overview.indexOf("health-layout");
  const usage = overview.indexOf("usage-section");
  const info = overview.indexOf("gatewayInfo");
  assert(heading > -1 && health > -1 && usage > -1 && info > -1, "总览必须包含运行状态、用量与运行信息");
  assert(heading < health && health < usage, "运行状态必须排在用量与性能之前");
  assert(usage < info, "运行信息仍应排在最后");
  audit("总览区块顺序通过", {health_first: true, usage_after_health: true});
});


test("账号日志告警分级、脱敏文本转义、已恢复记录剔除和登录退出清理", () => {
  const f = fixture();
  const alerts = [
    {account: "paused.json", state: "paused", severity: "notice", label: "账号停用", message: "主动停用", at: 1791980002, logFile: "gateway.log"},
    {account: "dead<script>.json", state: "disabled", severity: "urgent", label: "账号失效", message: '<img src=x onerror="alert(1)">', at: 1791980001, logFile: "gateway.log"},
    {account: "expired.json", state: "expired", severity: "important", label: "账号过期", message: "请检查续期", at: 1791980000, logFile: "gateway-2026-10-14.log"}
  ];
  f.review.state.lastStatus = {accounts: [
    {path: "expired.json", state: "active"},
    {path: "dead<script>.json", state: "disabled"},
    {path: "paused.json", state: "active", schedulingEnabled: false}
  ]};
  f.review.renderAccountAlerts({alerts, source: "gateway 普通日志"});
  const html = f.element("accountAlerts").innerHTML;
  assert(!html.includes("expired.json"), "恢复后不能继续展示过期告警");
  assert(html.includes("alert-urgent") && html.includes("紧急"));
  assert(html.includes("alert-notice") && html.includes("提示"));
  assert(html.includes("&lt;script&gt;") && !html.includes("<img"));
  assert(html.indexOf("paused.json") < html.indexOf("dead&lt;script&gt;.json"), "较新的提示告警必须在较旧紧急告警之前，前端不得重新按等级排序");
  assert.match(f.element("accountAlertsInfo").textContent, /最新告警在上/);
  assert(!f.element("accountAlertsInfo").textContent.includes("按紧急程度排序"));
  f.review.renderAccountAlerts({alerts: [], missingLogCount: 1, scanTruncated: true});
  assert.match(f.element("accountAlerts").innerHTML, /暂未找到对应日志/);
  assert.match(f.element("accountAlertsInfo").textContent, /读取上限/);
  f.review.lockDashboard();
  assert.equal(f.review.state.lastAccountAlerts, null);
  assert(!f.element("accountAlerts").innerHTML.includes("dead"));
  audit("账号告警展示通过", {recovered_removed: true, escaped: true, logout_cleared: true});
});

test("新状态快照恢复后立即剔除旧告警，无需等待再次扫描日志", () => {
  const f = fixture();
  const payload = {alerts: [
    {account: "cooling.json", state: "cooldown", severity: "warning", label: "账号冷却",
      message: "等待恢复", at: 1791980005, logFile: "gateway.log"},
    {account: "paused.json", state: "paid_exhausted", severity: "important", label: "积分用尽",
      message: "等待额度", at: 1791980004, logFile: "gateway.log"},
    {account: "paused.json", state: "paused", severity: "notice", label: "账号停用",
      message: "主动停用", at: 1791980003, logFile: "gateway.log"}
  ]};
  f.review.state.lastStatus = {accounts: [
    {path: "cooling.json", state: "cooldown"},
    {path: "paused.json", state: "paid_exhausted", schedulingEnabled: false}
  ]};
  f.review.renderAccountAlerts(payload);
  assert.match(f.element("accountAlerts").innerHTML, /账号冷却/);
  assert.match(f.element("accountAlerts").innerHTML, /积分用尽/);
  // renderStatus re-filters the existing payload on the normal status poll.
  f.review.renderStatus({accounts: [
    {path: "cooling.json", state: "active"},
    {path: "paused.json", state: "active", schedulingEnabled: false}
  ]});
  const html = f.element("accountAlerts").innerHTML;
  assert(!html.includes("账号冷却") && !html.includes("积分用尽"));
  assert(html.includes("账号停用"), "额度恢复不能自动撤销用户主动停用");
  audit("新快照剔除已恢复告警通过", {log_rescan_required: false, manual_pause_preserved: true});
});

test("告警日志加载失败保留旧结果，不假报无异常", async () => {
  const f = fixture(async route => route === "/admin/api/client-events" ? response(204, {}) : response(503, {error: "日志不可读"}));
  f.review.state.apiKey = "synthetic-admin";
  f.element("accountAlerts").innerHTML = "原有告警";
  await f.review.loadAccountAlerts();
  assert.equal(f.element("accountAlerts").innerHTML, "原有告警");
  assert.match(f.element("accountAlertsInfo").textContent, /不代表账号池没有异常/);
  assert.equal(f.review.state.accountAlertsPending, false);
  audit("告警加载失败保留结果通过");
});

test("已知零值显示0，未知额度仍显示横线", () => {
  const f = fixture();
  f.review.renderAccounts([{path: "fixture.json", state: "active", quotaKnown: true}]);
  let row = cells(f.element("accountsTable").innerHTML);
  for (const i of [7, 8, 9, 11, 12, 15]) assert.equal(row[i], "0");
  f.review.renderAccounts([{path: "fixture.json", state: "active", quotaKnown: false}]);
  row = cells(f.element("accountsTable").innerHTML);
  for (const i of [7, 8, 9]) assert.equal(row[i], "-");
  f.review.renderModels({models: [{id: "fixture-model"}]});
  assert.equal(cells(f.element("modelsTable").innerHTML)[4], "0");
  audit("零值展示通过", {unknown_quota_preserved: true});
});

test("企业不限量显示不限，已用保留小数；未知和有限额度不混淆", () => {
  const f = fixture();
  f.review.renderAccounts([{path: "enterprise.json", state: "active", quotaKnown: true,
    quotaUnlimited: true, quotaTotal: -1, quotaUsed: 12.5, quotaRemaining: -1, planLabel: "企业版"}]);
  let row = cells(f.element("accountsTable").innerHTML);
  assert.equal(row[7], "不限");
  assert.equal(row[8], "12.5");
  assert.equal(row[9], "不限");
  assert.equal(row[10], "企业版");
  f.review.renderAccounts([{path: "enterprise.json", state: "active", quotaKnown: false, quotaUnlimited: true}]);
  row = cells(f.element("accountsTable").innerHTML);
  for (const i of [7, 8, 9]) assert.equal(row[i], "-");
  audit("企业额度展示通过", {unlimited_distinct_from_unknown: true, fractional_used_preserved: true});
});

test("快照503保留上次内容，不生成零账号", async () => {
  const f = fixture(async route => route === "/admin/api/client-events" ? response(204, {}) : response(503, {error: "状态快照暂不可用"}));
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.renderStatus({gateway: {}, accounts: [{path: "existing.json", state: "active"}]});
  const before = f.element("accountsTable").innerHTML;
  await assert.rejects(f.review.api("/admin/api/status"));
  assert.equal(f.element("accountsTable").innerHTML, before);
  assert.match(f.element("alert").textContent, /503.*已有数据保留/);
  audit("快照失败保留旧数据通过");
});

test("轮询不会重叠，前后端审计沿用同一个TraceID", async () => {
  let release;
  const calls = [];
  const f = fixture((route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return Promise.resolve(response(204, {}));
    return new Promise(resolve => { release = resolve; });
  });
  f.review.state.apiKey = "synthetic-admin-key";
  const first = f.review.refreshActive();
  await f.review.refreshActive();
  assert.equal(calls.filter(c => c.route === "/admin/api/status").length, 1);
  release(response(200, {gateway: {}, accounts: [], summary: {total: 0}}));
  await first;
  assert.equal(f.review.state.refreshPending, false);
  const event = calls.find(c => c.route === "/admin/api/client-events");
  assert.equal(event.options.headers["X-Trace-ID"], calls[0].options.headers["X-Trace-ID"]);
  assert(!event.options.body.includes("synthetic-admin-key"));
  audit("轮询与审计通过", {overlapping_status_requests: 0});
});

test("错误管理Key不保存到会话存储", async () => {
  const f = fixture(async () => response(401, {error: "管理鉴权失败"}));
  f.element("apiKey").value = "wrong-synthetic-admin";
  await f.review.saveKey();
  assert.equal(f.session.size, 0);
  assert.equal(f.review.state.apiKey, "");
  audit("错误登录不持久保存通过");
});

test("首次初始化只弹窗一次，并使用返回的管理Key登录", async () => {
  const key = "a".repeat(32);
  let setups = 0;
  const f = fixture(async (route, options) => {
    if (route === "/admin/api/setup") {
      assert.equal(options.method, "POST");
      assert.equal(options.headers["X-Workbuddy-Admin"], "1");
      setups++;
      return response(201, {adminKey: key});
    }
    if (route === "/admin/api/settings") return response(200, {apiKeyEnabled: false, apiKeyConfigured: false, apiPort: 8317, webPort: 8316});
    if (route === "/admin/api/status") return response(200, {gateway: {}, accounts: []});
    return response(204, {});
  });
  await f.review.initializeAdmin();
  assert.equal(setups, 1);
  assert.equal(f.alerts.length, 1);
  assert(f.alerts[0].includes(key));
  assert.equal(f.session.get("wb_gateway_admin_key"), key);
  assert.equal(f.local.size, 0);
  audit("初始化弹窗与登录通过", {key_length: key.length, saved_to_localStorage: false});
});

test("没有API Key时阻止开启；关闭开关可以保存", async () => {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/settings") return response(200, {apiKeyEnabled: false, apiKeyConfigured: false});
    return response(204, {});
  });
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.settings = {apiKeyConfigured: false};
  f.element("apiKeyEnabled").checked = true;
  f.review.saveAPISettings();
  assert.equal(calls.filter(c => c.route === "/admin/api/settings").length, 0);
  assert.match(f.element("alert").textContent, /必须输入/);
  f.element("apiKeyEnabled").checked = false;
  f.review.saveAPISettings();
  await new Promise(resolve => setImmediate(resolve));
  const saved = calls.find(c => c.route === "/admin/api/settings");
  assert.equal(JSON.parse(saved.options.body).apiKeyEnabled, false);
  audit("API鉴权开关表单通过");
});

const configContent = JSON.stringify({gateway: {adminKey: "synthetic-admin-key", apiKey: "synthetic-private-api-key"}, systemPrompt: {force: "synthetic-private-prompt"}}, null, 2);
function configFixture(fetchImpl) {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return response(204, {});
    if (fetchImpl) {
      const result = await fetchImpl(route, options);
      if (result) return result;
    }
    if (route.startsWith("/admin/api/config-edit?")) return response(200, {content: configContent, revision: "original-revision"});
    if (route === "/admin/api/config") return response(200, {exists: true, content: "masked preview"});
    if (route === "/admin/api/settings") return response(200, {apiKeyConfigured: true, apiPort: 8317, webPort: 8316});
    return response(200, {saved: true, adminKeyChanged: false, settings: {}});
  });
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.activeTab = "config";
  return {...f, calls};
}
async function openConfigEditor(f) {
  f.review.showConfigWarning();
  await f.review.loadConfigEditor();
}

test("配置编辑先警告，确认后才请求真实原文，审计不含正文", async () => {
  const f = configFixture();
  await f.review.loadConfigEditor();
  assert.equal(f.calls.length, 0, "未进入警告状态不应加载明文");
  f.review.showConfigWarning();
  assert.equal(f.review.state.configMode, "warning");
  assert.equal(f.calls.filter(c => c.route.startsWith("/admin/api/config-edit")).length, 0);
  await f.review.loadConfigEditor();
  assert.equal(f.review.state.configMode, "edit");
  assert.equal(f.element("configEditor").value, configContent);
  assert.equal(f.local.size, 0);
  assert.equal(f.session.size, 0);
  const request = f.calls.find(c => c.route.includes("?acknowledged=1"));
  const acknowledged = f.calls.find(c => c.route === "/admin/api/client-events" && JSON.parse(c.options.body).event === "config_edit_acknowledged");
  assert.equal(request.options.headers["X-Trace-ID"], acknowledged.options.headers["X-Trace-ID"]);
  for (const event of f.calls.filter(c => c.route === "/admin/api/client-events")) {
    assert(!event.options.body.includes("synthetic-private"));
  }
  audit("编辑警告与明文加载通过", {persistent_config: false, trace_shared: true});
});

test("编辑器拒绝无效JSON，不提交且保留输入", async () => {
  const f = configFixture();
  await openConfigEditor(f);
  for (const input of ["{", "[]", "null", JSON.stringify({value: "汉".repeat(360000)})]) {
    f.element("configEditor").value = input;
    await f.review.saveConfigEditor();
    assert.equal(f.calls.filter(c => c.route === "/admin/api/config-edit" && c.options.method === "POST").length, 0);
    assert.equal(f.element("configEditor").value, input);
    assert.match(f.element("configEditorStatus").textContent, /配置未保存/);
  }
  audit("编辑器本地校验通过");
});

test("版本冲突和服务器失败保留修改，不偷偷刷新覆盖", async () => {
  for (const status of [409, 500]) {
    const f = configFixture(async (route, options) => route === "/admin/api/config-edit" && options.method === "POST" ? response(status, {error: "保存未完成"}) : null);
    await openConfigEditor(f);
    const edited = configContent.replace("synthetic-private-prompt", "edited-private-prompt");
    f.element("configEditor").value = edited;
    await f.review.saveConfigEditor();
    assert.equal(f.element("configEditor").value, edited);
    assert.equal(f.review.state.configRevision, "original-revision");
    assert.equal(f.review.state.configPending, false);
    assert.equal(f.element("cancelConfigEdit").disabled, false);
    assert.match(f.element("configEditorStatus").textContent, status === 409 ? /版本冲突/ : /保存未完成/);
    assert.equal(f.calls.filter(c => c.route.includes("?acknowledged=1")).length, 1);
  }
  audit("冲突和服务器失败输入保留通过");
});

test("取消加载后忽略迟到响应，不重新暴露Key", async () => {
  let release;
  const f = configFixture((route) => route.includes("?acknowledged=1") ? new Promise(resolve => { release = resolve; }) : null);
  f.review.showConfigWarning();
  const loading = f.review.loadConfigEditor();
  assert.equal(f.review.exitConfigEdit(), true);
  release(response(200, {content: configContent, revision: "late-revision"}));
  await loading;
  assert.equal(f.element("configEditor").value, "");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.review.state.configRevision, "");
  assert.equal(f.review.state.configMode, "preview");
  audit("取消后迟到响应隔离通过");
});

test("离开配置页先确认丢弃修改，并清除所有编辑原文", async () => {
  const f = configFixture();
  await openConfigEditor(f);
  f.element("configEditor").value += " ";
  f.window.confirm = () => false;
  f.review.setTab("settings");
  assert.equal(f.review.state.activeTab, "config");
  assert(f.element("configEditor").value.includes("synthetic-private-api-key"));
  f.window.confirm = () => true;
  f.review.setTab("settings");
  assert.equal(f.review.state.activeTab, "settings");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.element("configEditor").value, "");
  audit("离开页面清除编辑原文通过");
});

test("保存成功回到脱敏预览，端口待重启提示保留", async () => {
  const f = configFixture(async (route, options) => route === "/admin/api/config-edit" && options.method === "POST" ?
    response(200, {saved: true, adminKeyChanged: false, settings: {restartRequired: true}}) : null);
  await openConfigEditor(f);
  await f.review.saveConfigEditor();
  assert.equal(f.element("configEditor").value, "");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.review.state.configMode, "preview");
  assert.match(f.element("configResult").textContent, /需重启/);
  const post = f.calls.find(c => c.route === "/admin/api/config-edit" && c.options.method === "POST");
  const body = JSON.parse(post.options.body);
  assert.equal(body.content, configContent);
  assert.equal(body.revision, "original-revision");
  assert.equal(body.acknowledged, true);
  audit("保存与端口重启提示通过");
});

test("管理Key修改后清除编辑原文和旧会话，要求重新登录", async () => {
  const f = configFixture(async (route, options) => route === "/admin/api/config-edit" && options.method === "POST" ?
    response(200, {saved: true, adminKeyChanged: true, settings: {}}) : null);
  f.session.set("wb_gateway_admin_key", "synthetic-admin-key");
  await openConfigEditor(f);
  f.element("configEditor").value = configContent.replace("synthetic-admin-key", "new-private-admin-key");
  await f.review.saveConfigEditor();
  assert.equal(f.review.state.apiKey, "");
  assert.equal(f.session.size, 0);
  assert.equal(f.element("configEditor").value, "");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.element("apiKey").value, "");
  assert.match(f.element("alert").textContent, /使用新 Key 重新登录/);
  audit("管理Key变更清除会话通过");
});

test("清空管理Key需要额外确认，取消不发起保存", async () => {
  const f = configFixture();
  await openConfigEditor(f);
  f.element("configEditor").value = "{}";
  f.window.confirm = () => false;
  await f.review.saveConfigEditor();
  assert.equal(f.calls.filter(c => c.route === "/admin/api/config-edit" && c.options.method === "POST").length, 0);
  assert.equal(f.element("configEditor").value, "{}");
  audit("管理Key清空额外确认通过");
});

test("账号池筛选只保留可用且已开启调度的账号，取消后恢复全部", () => {
  const f = fixture();
  f.review.initListFilters();
  const accounts = [
    {path: "active.json", state: "active", schedulingEnabled: true},
    {path: "paused.json", state: "active", schedulingEnabled: false},
    {path: "cooldown.json", state: "cooldown", schedulingEnabled: true},
    {path: "expired.json", state: "expired"},
    {path: "disabled.json", state: "disabled"},
    {path: "exhausted.json", state: "paid_exhausted"},
  ];
  f.review.renderStatus({gateway: {}, accounts});
  assert(f.element("accountsTable").innerHTML.includes("paused.json"));
  f.element("onlyUsableAccounts").checked = true;
  f.review.changeListFilter("accounts");
  let html = f.element("accountsTable").innerHTML;
  assert(html.includes("active.json"));
  for (const name of ["paused", "cooldown", "expired", "disabled", "exhausted"]) assert(!html.includes(name + ".json"));
  assert.equal(f.review.state.lastStatus.accounts.length, 6);
  f.element("onlyUsableAccounts").checked = false;
  f.review.changeListFilter("accounts");
  assert(f.element("accountsTable").innerHTML.includes("paused.json"));
  assert.equal(accounts.length, 6);
  audit("账号筛选通过", {snapshot_mutated: false});
});

test("模型筛选要求已启用且有可用账号，空结果解释筛选条件", () => {
  const f = fixture();
  f.review.initListFilters();
  const data = {models: [
    {id: "available", enabled: true, availableAccounts: 2},
    {id: "no-accounts", enabled: true, availableAccounts: 0},
    {id: "disabled-model", enabled: false, availableAccounts: 2},
  ]};
  f.review.renderModels(data);
  f.element("onlyUsable").checked = true;
  f.review.changeListFilter("models");
  assert(f.element("modelsTable").innerHTML.includes("available"));
  assert(!f.element("modelsTable").innerHTML.includes("no-accounts"));
  assert(!f.element("modelsTable").innerHTML.includes("disabled-model"));
  f.review.renderModels({models: [{id: "unavailable", availableAccounts: 0}]});
  assert.match(f.element("modelsTable").innerHTML, /取消勾选可查看全部模型/);
  f.review.renderAccounts([{path: "paused.json", state: "active", schedulingEnabled: false}]);
  f.element("onlyUsableAccounts").checked = true;
  f.review.renderAccounts([{path: "paused.json", state: "active", schedulingEnabled: false}]);
  assert.match(f.element("accountsTable").innerHTML, /取消勾选可查看全部账号/);
  audit("模型筛选与空结果通过");
});

test("两个Cookie分别记住状态，刷新及重新登录不丢失，无凭据入Cookie", async () => {
  const jar = new Map();
  const f = fixture(async () => response(204, {}), jar);
  f.review.initListFilters();
  f.review.state.apiKey = "synthetic-private-admin-key";
  f.element("onlyUsableAccounts").checked = true;
  f.review.changeListFilter("accounts");
  f.element("onlyUsable").checked = false;
  f.review.changeListFilter("models");
  assert.equal(jar.get("gateway_accounts_only_usable"), "1");
  assert.equal(jar.get("gateway_models_only_usable"), "0");
  assert.equal(jar.size, 2);
  for (const value of f.cookieWrites) {
    assert.match(value, /Path=\/ui; Max-Age=31536000; SameSite=Lax/);
    assert(!value.includes("synthetic-private-admin-key"));
  }
  f.review.lockDashboard();
  assert.equal(jar.get("gateway_accounts_only_usable"), "1");
  const reloaded = fixture(undefined, jar);
  reloaded.review.initListFilters();
  assert.equal(reloaded.element("onlyUsableAccounts").checked, true);
  assert.equal(reloaded.element("onlyUsable").checked, false);
  reloaded.element("onlyUsable").checked = true;
  reloaded.review.changeListFilter("models");
  reloaded.element("onlyUsableAccounts").checked = false;
  reloaded.review.changeListFilter("accounts");
  const secondReload = fixture(undefined, jar);
  secondReload.review.initListFilters();
  assert.equal(secondReload.element("onlyUsableAccounts").checked, false);
  assert.equal(secondReload.element("onlyUsable").checked, true);
  assert.equal(secondReload.local.size, 0);
  audit("Cookie独立记忆与刷新恢复通过", {secret_cookie: false, saved_to_localStorage: false});
});

test("缺失或非法Cookie默认显示全部，HTTPS添加Secure属性", () => {
  const f = fixture(undefined, new Map([
    ["gateway_accounts_only_usable", "true"],
    ["gateway_models_only_usable", "invalid"],
  ]));
  f.review.initListFilters();
  assert.equal(f.element("onlyUsableAccounts").checked, false);
  assert.equal(f.element("onlyUsable").checked, false);
  f.window.location.protocol = "https:";
  f.element("onlyUsable").checked = true;
  f.review.changeListFilter("models");
  assert.match(f.cookieWrites[0], /; Secure$/);
  audit("非法Cookie回落与HTTPS属性通过");
});

test("Cookie被禁用仍立即筛选，并提示刷新无法记住；审计沿用同一TraceID", () => {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    return response(204, {});
  });
  Object.defineProperty(f.document, "cookie", {
    get() { throw new Error("cookie blocked"); },
    set() { throw new Error("cookie blocked"); },
  });
  f.review.initListFilters();
  f.review.state.apiKey = "synthetic-private-admin-key";
  f.review.renderStatus({gateway: {}, accounts: [
    {path: "available.json", state: "active"},
    {path: "paused.json", state: "active", schedulingEnabled: false},
  ]});
  f.element("onlyUsableAccounts").checked = true;
  f.review.changeListFilter("accounts");
  assert(!f.element("accountsTable").innerHTML.includes("paused.json"));
  assert.match(f.element("accountFilterStatus").textContent, /浏览器未允许保存 Cookie/);
  assert(!f.element("accountFilterStatus").classList.contains("hidden"));
  assert.equal(calls.length, 2);
  const records = calls.map(c => JSON.parse(c.options.body));
  assert.deepEqual(records.map(r => r.event), ["list_filter_changed", "list_filter_cookie_failed"]);
  assert(records.every(r => r.enabled === true && r.route === "/admin/api/status"));
  assert.equal(calls[0].options.headers["X-Trace-ID"], calls[1].options.headers["X-Trace-ID"]);
  assert(calls.every(c => !c.options.body.includes("synthetic-private-admin-key")));
  audit("Cookie禁用回落与审计通过");
});

test("请求进行中切换筛选也会按最新勾选状态渲染，不额外获取数据", async () => {
  let release;
  const calls = [];
  const f = fixture((route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return Promise.resolve(response(204, {}));
    return new Promise(resolve => { release = resolve; });
  });
  f.review.initListFilters();
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.activeTab = "models";
  const pending = f.review.refreshActive();
  f.element("onlyUsable").checked = true;
  f.review.changeListFilter("models");
  release(response(200, {models: [{id: "available", availableAccounts: 2}, {id: "empty", availableAccounts: 0}]}));
  await pending;
  assert(f.element("modelsTable").innerHTML.includes("available"));
  assert(!f.element("modelsTable").innerHTML.includes("empty"));
  assert.equal(calls.filter(c => c.route === "/admin/api/models").length, 1);
  audit("在途请求按最新筛选渲染通过", {additional_data_requests: 0});
});

test("网页静态文案移除旧品牌，技术鉴权Header保持兼容", () => {
  const html = fs.readFileSync(path.join(projectRoot, "web", "index.html"), "utf8");
  assert(!/workbuddy/i.test(html));
  assert.match(html, /<title>本地网关 · 网页控制台<\/title>/);
  assert.match(html, /id="onlyUsableAccounts"/);
  assert.match(html, /只显示可用账号/);
  assert.match(html, /只显示可用模型/);
  const script = fs.readFileSync(path.join(projectRoot, "web", "app.js"), "utf8");
  assert.match(script, /X-Workbuddy-Admin/);
  audit("去品牌文案与协议兼容通过");
});

function rateFixture(payload) {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return response(204, {});
    if (route === "/admin/api/rate-detail") return response(200, payload);
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin-key";
  return {...f, calls};
}

function fakeRateButton(site, model) {
  const details = {open: false, handler: null,
    addEventListener(type, fn) { if (type === "toggle") this.handler = fn; }};
  const panel = {innerHTML: "", className: "rate-detail hidden",
    querySelector(sel) { return sel === "details.rate-raw" ? details : null; },
    classList: {add(n) { this.owner.className += " " + n; }, remove(n) {
      this.owner.className = this.owner.className.split(/\s+/).filter(v => v !== n).join(" ");
    }, owner: null}};
  panel.classList.owner = panel;
  const cell = {querySelector: sel => sel === ".rate-detail" ? panel : null};
  const button = {
    attrs: {"data-rate-site": site, "data-rate-model": model, "aria-expanded": "false"},
    getAttribute(name) { return this.attrs[name] === undefined ? null : this.attrs[name]; },
    setAttribute(name, value) { this.attrs[name] = value; },
    closest(sel) { return sel === "td" ? cell : (sel === "button.rate-info" ? this : null); },
  };
  return {button, panel, details};
}

test("倍率单元格在数值后渲染感叹号按钮，模型名带引号也不会破坏属性", () => {
  const f = rateFixture({rates: {}});
  const html = f.review.rateCell("intl", "hy4-preview", "收费(倍率未知)");
  assert.match(html, /class='rate-value'>收费\(倍率未知\)</);
  assert.match(html, /class='rate-info'/);
  assert.match(html, /data-rate-site='intl'/);
  assert.match(html, /data-rate-model='hy4-preview'/);
  assert.match(html, /aria-expanded='false'/);
  assert.match(html, /class='rate-detail hidden'/);
  assert.equal(f.review.rateKey("intl", "  HY4-Preview "), "intl|hy4-preview");
  f.review.renderModels({models: [{id: "hy4-preview", cnMultiplier: "0.29x", intlMultiplier: "收费(倍率未知)"}]});
  const table = f.element("modelsTable").innerHTML;
  assert.equal((table.match(/class='rate-info'/g) || []).length, 2);
  assert(table.includes("0.29x") && table.includes("收费(倍率未知)"));
  audit("倍率感叹号渲染通过");
});

test("已知倍率用白话解释，未知额外给出如何刷新，原始日志默认折叠", () => {
  const f = rateFixture({rates: {}});
  const rawLine = "2026/10/13 10:59:36 [Models] 官方倍率 站点=cn 模型=hy3 生效倍率=0.29 说明=很长的一整行原始日志";
  const known = f.review.rateDetailHTML({rates: {"cn|hy3": {
    site: "cn", model: "hy3", source: "官方目录（实时接口）", updatedAt: 1791860376,
    value: "0.29x", explain: "官方接口给出的目录倍率", unknown: false, hasLog: true,
    records: [{source: "官方目录（实时接口）", updatedAt: 1791860376, value: "0.29x",
      explain: "官方接口给出的目录倍率", logLine: rawLine}],
  }}}, "cn", "hy3");
  assert.match(known, /来源：官方目录（实时接口）/);
  assert.match(known, /更新时间：/);
  assert.match(known, /官方接口给出的目录倍率/);
  assert(!known.includes("如何刷新"), "已知倍率不需要刷新指引");
  assert.match(known, /原始日志（1 条，默认折叠）/);
  const rawAt = known.indexOf("<code>");
  const detailsAt = known.indexOf("<details");
  const detailsEnd = known.indexOf("</details>");
  assert(detailsAt >= 0 && rawAt > detailsAt && rawAt < detailsEnd, "原始日志必须放在折叠块里");
  const unknown = f.review.rateDetailHTML({rates: {"intl|hy4-preview": {
    site: "intl", model: "hy4-preview", source: "价格探测（缓存结论）", updatedAt: 1791860376,
    value: "收费(倍率未知)", explain: "上次真实探测结论：收费（尚未复测）",
    refreshHint: "后台自动探测已关闭。用该站点的账号请求一次这个模型，网关会按返回的 usage.credit 自动重新判定并写入日志。",
    unknown: true, hasLog: true,
    records: [{source: "价格探测（缓存结论）", updatedAt: 1791860376, value: "收费(倍率未知)",
      explain: "上次真实探测结论：收费（尚未复测）", logLine: rawLine}],
    others: [{source: "官方目录（实时接口）", updatedAt: 1791860370, explain: "官方促销已于 2026-09-08 结束，接口仍返回 0.00x，该数值不可信"}],
  }}}, "intl", "hy4-preview");
  assert(!unknown.includes("状态：未知"), "已实测过的结论不应标注为未知");
  assert.match(unknown, /上次真实探测结论：收费（尚未复测）/);
  assert.match(unknown, /如何刷新：/);
  assert.match(unknown, /请求一次这个模型/);
  assert.match(unknown, /其他记录/);
  assert.match(unknown, /官方促销已于 2026-09-08 结束/);
  const missing = f.review.rateDetailHTML({rates: {}, noLogHint: "普通日志中没有该模型的记录。用该站点的账号请求一次这个模型，就会产生新的倍率记录。"}, "intl", "not-logged-model");
  assert.match(missing, /没有日志/);
  assert.match(missing, /请求一次这个模型/);
  assert(!missing.includes("来源："));
  audit("倍率白话解释与刷新指引通过");
});

test("点击感叹号按需拉取日志详情并复用缓存，失败不伪造来源", async () => {
  const payload = {rates: {"cn|hy3": {site: "cn", model: "hy3", source: "官方目录（实时接口）",
    updatedAt: 1791860376, value: "0.29x", unknown: false, hasLog: true, records: []}}};
  const f = rateFixture(payload);
  const first = fakeRateButton("cn", "hy3");
  f.review.toggleRateDetail({target: first.button});
  assert.equal(first.button.getAttribute("aria-expanded"), "true");
  assert.equal(f.review.state.openRateKeys["cn|hy3"], true);
  await new Promise(resolve => setImmediate(resolve));
  assert.match(first.panel.innerHTML, /来源：官方目录/);
  assert.equal(f.calls.filter(c => c.route === "/admin/api/rate-detail").length, 1);
  // 第二次展开同一页面的另一个倍率不应重复请求日志。
  const second = fakeRateButton("intl", "hy4-preview");
  f.review.toggleRateDetail({target: second.button});
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.calls.filter(c => c.route === "/admin/api/rate-detail").length, 1);
  assert.match(second.panel.innerHTML, /没有日志/);
  // 再次点击折叠，并清除展开状态。
  f.review.toggleRateDetail({target: first.button});
  assert.equal(first.button.getAttribute("aria-expanded"), "false");
  assert.equal(f.review.state.openRateKeys["cn|hy3"], undefined);
  assert.equal(first.panel.innerHTML, "");
  audit("倍率详情按需拉取与缓存通过", {rate_detail_requests: 1});
});

test("轮询重建表格后，已展开的倍率详情自动恢复", async () => {
  const payload = {rates: {"cn|hy3": {site: "cn", model: "hy3", source: "官方目录（实时接口）",
    updatedAt: 1791860376, value: "0.29x", unknown: false, hasLog: true, records: []}}};
  const f = rateFixture(payload);
  f.review.state.openRateKeys["cn|hy3"] = true;
  f.review.ensureRateDetails();
  await new Promise(resolve => setImmediate(resolve));
  const buttons = [];
  const table = {querySelectorAll: () => buttons};
  const cell = {querySelector: sel => sel === ".rate-detail" ? panel : null};
  const panel = {innerHTML: "", className: "rate-detail hidden",
    querySelector: () => null,
    classList: {add(n) { this.owner.className += " " + n; }, remove(n) {
      this.owner.className = this.owner.className.split(/\s+/).filter(v => v !== n).join(" ");
    }, owner: null}};
  panel.classList.owner = panel;
  const button = {
    attrs: {"data-rate-site": "cn", "data-rate-model": "hy3", "aria-expanded": "false"},
    getAttribute(name) { return this.attrs[name]; },
    setAttribute(name, value) { this.attrs[name] = value; },
    closest(sel) { return sel === "td" ? cell : null; },
  };
  buttons.push(button);
  f.review.restoreRateDetails(table);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(button.getAttribute("aria-expanded"), "true");
  assert.match(panel.innerHTML, /来源：官方目录/);
  audit("轮询后详情恢复通过");
});

test("展开原始日志后，轮询刷新不会把它自动折叠", async () => {
  const rawLine = "2026/10/13 15:28:00 [Models] 官方倍率 站点=cn 模型=hy3 生效倍率=0.29";
  const payload = {rates: {"cn|hy3": {site: "cn", model: "hy3", source: "官方目录（实时接口）",
    updatedAt: 1791860376, value: "0.29x", explain: "官方接口给出的目录倍率", unknown: false, hasLog: true,
    records: [{source: "官方目录（实时接口）", updatedAt: 1791860376, value: "0.29x",
      explain: "官方接口给出的目录倍率", logLine: rawLine}]}}};
  const f = rateFixture(payload);
  const {button, details} = fakeRateButton("cn", "hy3");
  f.review.toggleRateDetail({target: button});
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(typeof details.handler, "function", "渲染后必须绑定 toggle 监听");
  details.open = true;
  details.handler();
  assert.equal(f.review.state.openRawLogs["cn|hy3"], true);
  // 轮询会重建整张表格，重建后的 HTML 必须保持展开。
  assert.match(f.review.rateDetailHTML(payload, "cn", "hy3"), /<details class='rate-raw' open>/);
  details.open = false;
  details.handler();
  assert.equal(f.review.state.openRawLogs["cn|hy3"], undefined);
  assert(!/<details class='rate-raw' open>/.test(f.review.rateDetailHTML(payload, "cn", "hy3")));
  audit("原始日志展开状态在轮询后保持通过");
});

// ---- 总览 · 用量与性能 ------------------------------------------------------

function statsBucket(label, over = {}) {
  return Object.assign({start: 0, label, requests: 0, success: 0, failed: 0, tokens: 0, inputTokens: 0,
    outputTokens: 0, cachedTokens: 0, tokenSamples: 0, credits: 0, creditSamples: 0, ttftSamples: 0,
    avgLatencyMs: null, avgTtftMs: null}, over);
}

function statsPayload(over = {}) {
  return Object.assign({
    period: "day", account: "", timezone: "Asia/Shanghai", ttftMetric: "first_token_v2", start: "2026-09-15", end: "2026-10-14",
    startedAt: 1760000000000, accounts: [{id: "acc-a", file: "workbuddy-a.json", edition: "cn"}],
    summary: {requests: 2, success: 1, failed: 1, tokens: 300, inputTokens: 200, outputTokens: 100,
      cachedTokens: 50, tokenSamples: 1, credits: 1.25, creditSamples: 1, ttftSamples: 1,
      avgLatencyMs: 1200, avgTtftMs: 300},
    buckets: [statsBucket("2026-10-13", {requests: 1, success: 1, tokens: 300, inputTokens: 200,
      outputTokens: 100, cachedTokens: 50, tokenSamples: 1, credits: 1.25, creditSamples: 1,
      ttftSamples: 1, avgLatencyMs: 1200, avgTtftMs: 300}), statsBucket("2026-10-14")],
    models: [
      {model: "deepseek-v4.1-flash", requests: 3, success: 3, failed: 0, tokens: 900000,
        inputTokens: 700000, outputTokens: 200000, cachedTokens: 100000, tokenSamples: 3,
        credits: 4.5, creditSamples: 3, ttftSamples: 3, avgLatencyMs: 1500, avgTtftMs: 400},
      {model: "hy3", requests: 1, success: 1, failed: 0, tokens: 0, inputTokens: 0, outputTokens: 0,
        cachedTokens: 0, tokenSamples: 0, credits: 0, creditSamples: 0, ttftSamples: 1,
        avgLatencyMs: 800, avgTtftMs: 300},
    ],
    modelLatency: [
      {model: "deepseek-v4.1-flash", avgLatencyMs: [1500, null], avgTtftMs: [400, null]},
      {model: "hy3", avgLatencyMs: [null, 800], avgTtftMs: [null, 300]},
    ],
    storage: {mode: "file", message: "", lastSavedAt: 1760000000000},
  }, over);
}

test("TTFT新旧口径隔离：旧快照不伪装成首Token，明确显示排除数", () => {
  const f=fixture();
  const old=statsPayload({ttftMetric: undefined});
  old.summary.avgTtftMs=87190;
  f.review.renderStats(old);
  assert(!f.element("usageSummary").innerHTML.includes("87.19"));
  assert.match(f.element("usageSummary").innerHTML,/平均首Token/);
  assert.match(f.element("latencyLegend").innerHTML, /deepseek-v4\.1-flash/);
  assert(!f.element("latencyChart").innerHTML.includes("平均首Token"));
  const current=statsPayload();
  current.summary.legacyTtftSamples=12;
  f.review.renderStats(current);
  assert.match(f.element("statsInfo").textContent,/排除 12 条/);
  assert.match(f.element("statsInfo").textContent,/不等同于 dsh 当前会话/);
  assert.match(f.element("usageSummary").innerHTML,/0\.30 s/);
  f.review.renderModels({models:[{id:"legacy",avgTtftMs:87190,hasTtft:true}]});
  assert(!f.element("modelsTable").innerHTML.includes("87.19"));
  audit("首Token新旧样本隔离通过",{legacy_samples_excluded:true});
});

test("统计摘要区分未知与零值，并显示落盘状态", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const cards = f.element("usageSummary").innerHTML;
  assert.match(cards, /总消耗 Token/);
  assert.match(cards, /1\.25/, "积分必须按实际 usage.credit 展示");
  assert.match(cards, /1\/2 请求有积分/, "必须暴露积分样本覆盖数");
  assert.equal(f.element("statsStorage").textContent, "stats.json · 已落盘");
  // 无样本时显示未知，而不是 0。
  f.review.renderStats(statsPayload({summary: {requests: 1, success: 1, failed: 0, tokenSamples: 0,
    creditSamples: 0, ttftSamples: 0, avgLatencyMs: 900, avgTtftMs: null}}));
  const unknown = f.element("usageSummary").innerHTML;
  assert.match(unknown, /总消耗 Token<\/div><div class="usage-value">—</, "无 Token 样本不得显示为 0");
  assert.match(unknown, /总消耗积分<\/div><div class="usage-value">—</, "无积分样本不得显示为 0");
  // 仅内存模式必须给出明确提示，避免被当成已持久化。
  f.review.renderStats(statsPayload({storage: {mode: "memory", message: "统计暂时无法写入，仍在内存统计"}}));
  assert.equal(f.element("statsStorage").textContent, "仅内存 · 未落盘");
  assert.match(f.element("statsStorage").className, /warn/);
  assert.match(f.element("statsInfo").textContent, /统计暂时无法写入/);
  audit("统计摘要与落盘状态展示通过", {unknown_not_zero: true, memory_mode_flagged: true});
});

test("耗时折线在缺失样本处断开，不伪造零值", () => {
  const f = fixture();
  const svg = f.review.statsLineChart([
    statsBucket("2026-10-12", {avgLatencyMs: 1000, avgTtftMs: 200}),
    statsBucket("2026-10-13"),
    statsBucket("2026-10-14", {avgLatencyMs: 2000, avgTtftMs: 400}),
  ], [
    {label: "平均总耗时", color: "token", value: b => b.avgLatencyMs},
    {label: "平均首字", color: "first", value: b => b.avgTtftMs},
  ], "耗时", v => String(v));
  assert.match(svg, /<svg /);
  // 中间桶无样本 ⇒ 每序列各断开成两段，共 4 条折线路径。
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 2);
  assert.equal((svg.match(/class="chart-line series-first"/g) || []).length, 2);
  assert.equal((svg.match(/class="chart-line/g) || []).length, 4, "缺失样本处必须断线");
  assert.equal(f.review.statsLineChart([statsBucket("2026-10-14")], [
    {label: "平均总耗时", color: "token", value: b => b.avgLatencyMs},
  ], "耗时", String), '<p class="stats-empty">该时间范围暂无可用样本</p>', "全空必须显示空状态");
  audit("耗时折线断点与空状态通过", {no_fake_zero: true, path_segments: 4});
});

test("请求耗时按模型分线，不把所有模型画成一条平均线", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const svg = f.element("latencyChart").innerHTML;
  const legend = f.element("latencyLegend").innerHTML;
  assert(legend.indexOf("deepseek-v4.1-flash") < legend.indexOf("hy3"), "图例保持服务端顺序");
  assert.equal((svg.match(/class="chart-line series-m0"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line series-m1"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line /g) || []).length, 2, "每个模型一条线");
  assert.match(svg, /deepseek-v4\.1-flash 1\.50 s/);
  assert.match(svg, /hy3 0\.80 s/);
  assert(!svg.includes("series-token"), "不能再画所有模型合成的平均线");
  f.review.renderStats(statsPayload({modelLatency: []}));
  assert.match(f.element("latencyChart").innerHTML, /该时间范围暂无可用样本/);
  assert.equal(f.element("latencyLegend").textContent, "");
  audit("分模型耗时折线通过", {models: 2, blended_line: false});
});

test("模型用量排名按服务端顺序渲染，并跟随账号选择", () => {
  const f = fixture();
  f.review.renderStats(statsPayload({account: "acc-a"}));
  const rows = f.element("statsTable").querySelector("tbody").innerHTML;
  assert.match(rows, /deepseek-v4\.1-flash/);
  assert.match(rows, /hy3/);
  // 服务端已按用量排序，前端必须保持该顺序、不得重排。
  assert(rows.indexOf("deepseek-v4.1-flash") < rows.indexOf("hy3"), "用量多的模型必须排在前");
  assert.match(rows, /<td>1<\/td><td class='mono'>deepseek/, "必须显示排名序号");
  assert.match(rows, /Token 0\/1/, "无 usage 的模型仍要列出并标出覆盖数");
  assert(!/<td>\d{4}-\d{2}-\d{2}<\/td>/.test(rows), "明细不应再输出时间列");
  const select = f.element("statsAccount").innerHTML;
  assert.match(select, /全局 · 全部账号/);
  assert.match(select, /workbuddy-a\.json · 国内/);
  assert.equal(f.element("statsAccount").value, "acc-a", "必须保持当前选择");
  assert.equal(f.element("statsStart").value, "2026-09-15");
  assert.equal(f.element("statsEnd").value, "2026-10-14");
  // 空范围给出明确空态，而不是空表。放在最后，避免重置上面的选择断言。
  f.review.renderStats(statsPayload({models: []}));
  assert.match(f.element("statsTable").querySelector("tbody").innerHTML, /没有模型调用记录/);
  audit("模型用量排名渲染通过", {server_order_kept: true, no_time_column: true, empty_state: true});
});

// 回归：统计请求变慢或挂起时，状态轮询必须照常完成，不能被统计拖住。
test("统计加载不阻塞状态轮询", async () => {
  let releaseStatus;
  const routes = [];
  const f = fixture((route, options) => {
    routes.push(route);
    if (route === "/admin/api/client-events") return Promise.resolve(response(204, {}));
    if (route.startsWith("/admin/api/stats")) return new Promise(() => {}); // 永不返回
    return new Promise(resolve => { releaseStatus = resolve; });
  });
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.activeTab = "overview";
  const pending = f.review.refreshActive();
  releaseStatus(response(200, {gateway: {}, accounts: [], summary: {total: 0}}));
  await pending;
  assert.equal(f.review.state.refreshPending, false, "统计未返回不得让轮询一直处于 pending");
  assert(routes.some(r => r.startsWith("/admin/api/stats")), "总览页应触发一次统计请求");
  audit("统计不阻塞轮询通过", {stats_pending_isolated: true});
});

test("用量趋势合并为一张图：Token 折线 + 积分柱形，左右双轴", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const svg = f.element("usageChart").innerHTML;
  assert.match(svg, /class="chart-line series-token"/, "总 Token 必须画成折线");
  assert.match(svg, /class="chart-bar series-credit"/, "积分必须画成柱形");
  assert.equal((svg.match(/class="chart-line series-credit"/g) || []).length, 0, "积分不能再画折线");
  assert.equal((svg.match(/class="chart-bar series-token"/g) || []).length, 0, "Token 不能再画柱形");
  assert((svg.match(/class="chart-label"/g) || []).length >= 10, "Token 与积分量级不同，必须左右双轴");
  assert(!/NaN|undefined/.test(svg), "图表不能出现 NaN 或 undefined");
  audit("用量合并图通过", {token_line: true, credit_bar: true, dual_axis: true});
});

test("耗时趋势：首 Token 画柱形、总耗时画折线，且都按模型分开", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const svg = f.element("latencyChart").innerHTML;
  assert.equal((svg.match(/class="chart-line series-m0"/g) || []).length, 1, "总耗时仍是折线");
  assert.equal((svg.match(/class="chart-line series-m1"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-bar series-m0"/g) || []).length, 1, "首 Token 必须是柱形");
  assert.equal((svg.match(/class="chart-bar series-m1"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line /g) || []).length, 2, "折线数等于模型数");
  assert.equal((svg.match(/class="chart-bar /g) || []).length, 2, "柱形数等于模型数");
  assert(!svg.includes("series-token"), "耗时图不能混入用量系列色");
  audit("耗时柱线组合通过", {ttft_bar: true, latency_line: true, per_model: true});
});

test("双轴组合图：折线走左轴、柱形走右轴，缺样本处不补零", () => {
  const f = fixture();
  const svg = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 2000000, tokenSamples: 1, credits: 20, creditSamples: 1}),
    statsBucket("2026-10-14"),
    statsBucket("2026-10-15", {tokens: 4000000, tokenSamples: 1, credits: 40, creditSamples: 1}),
  ], {
    lines: [{label: "总 Token", color: "token", value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [{label: "积分", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "双轴", leftFormat: v => (v / 1000000).toFixed(2) + "M",
    rightFormat: v => Number(v).toFixed(2), box: {w: 600, h: 240}
  });
  assert.match(svg, /class="chart-line series-token"/);
  assert.match(svg, /class="chart-bar series-credit"/);
  assert((svg.match(/class="chart-label"/g) || []).length >= 10, "双轴必须有左右两套刻度");
  assert.match(svg, /M<\/text>/, "左轴按 Token 单位显示，右轴按积分精度");
  assert(!/NaN|undefined/.test(svg), "双轴不能出现 NaN 或 undefined");
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 2, "中间桶缺样本要断线");
  assert.equal((svg.match(/class="chart-bar series-credit"/g) || []).length, 2, "中间桶缺样本不画柱");
  assert(!f.review.statsComboChart([statsBucket("2026-10-14")], {
    lines: [], bars: [{label: "积分", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "空", leftFormat: String, box: {w: 600, h: 240}
  }).includes("<svg"), "全空必须显示空状态");
  audit("双轴组合图通过", {gap_broken: true, dual_axis: true});
});

test("柱形与折线对齐：柱组中心落在同桶折线点的时间位置", () => {
  const f = fixture();
  const svg = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 10, tokenSamples: 1, credits: 10, creditSamples: 1}),
    statsBucket("2026-10-14", {tokens: 20, tokenSamples: 1, credits: 20, creditSamples: 1}),
    statsBucket("2026-10-15", {tokens: 30, tokenSamples: 1, credits: 30, creditSamples: 1}),
  ], {
    lines: [{label: "总 Token", color: "token", value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [{label: "积分", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "对齐", leftFormat: String, box: {w: 600, h: 240}
  });
  const points = [...svg.matchAll(/<circle class="series-token" cx="([\d.]+)"/g)].map(m => Number(m[1]));
  const centres = [...svg.matchAll(/<rect class="chart-bar series-credit" x="([\d.]+)" y="[\d.]+" width="([\d.]+)"/g)]
    .map(m => Number(m[1]) + Number(m[2]) / 2);
  assert.equal(points.length, 3, "折线点数量");
  assert.equal(centres.length, 3, "柱形数量");
  points.forEach((cx, i) => {
    assert(Math.abs(centres[i] - cx) <= 1.5, "第 " + i + " 个柱组中心必须贴合同桶折线点: " + centres[i] + " vs " + cx);
  });
  audit("柱线对齐通过", {bars_centered_on_points: true});
});

test("账号池：添加账号表单复用登录流程，每行可删除凭据", async () => {
  const calls = [];
  const f = fixture(async (url, options) => {
    const method = (options && options.method) || "GET";
    calls.push(method + " " + url);
    if (url === "/admin/api/account-add" && method === "GET") return response(200, {defaultFile: "workbuddy4.json"});
    if (url === "/admin/api/account-add" && method === "POST") {
      return response(200, {id: "login-abc", site: "cn", file: "workbuddy4.json", status: "pending",
        authUrl: "https://example.test/auth?state=x", qrPng: "data:image/png;base64,AAAA"});
    }
    if (url.indexOf("/admin/api/account-add?id=") === 0) {
      return response(200, {id: "login-abc", status: "done", savedAs: "workbuddy4.json"});
    }
    if (url === "/admin/api/account-cancel") return response(200, {cancelled: true});
    if (url === "/admin/api/account-delete") return response(200, {deleted: "workbuddy9.json"});
    if (url === "/admin/api/status") return response(200, {gateway: {}, accounts: [], summary: {total: 0}});
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin";
  f.review.state.activeTab = "accounts";

  f.review.renderAccounts([{path: "workbuddy9.json", state: "active", schedulingEnabled: true, edition: "cn"}]);
  const row = f.element("accountsTable").querySelector("tbody").innerHTML;
  assert(row.includes('data-account-delete="workbuddy9.json"'), "每行必须有删除按钮并带出凭据名");

  await f.review.openAddAccount();
  assert.equal(f.element("addFile").value, "workbuddy4.json");
  assert(!f.element("addFile").value.includes("/"), "默认名不能含路径");

  await f.review.startAddAccount();
  assert.equal(f.review.state.addAccountId, "login-abc");
  assert.match(f.element("addAccountQr").src, /^data:image\/png;base64,/);
  assert.match(f.element("addAccountLink").textContent, /example\.test/);

  await f.review.pollAddAccount(true);
  assert.equal(f.review.state.addAccountId, "", "完成后必须清空会话，避免继续轮询");
  assert.match(f.element("addAccountStatus").textContent, /workbuddy4\.json/);
  assert(calls.some(c => c.indexOf("GET /admin/api/status") === 0), "登录成功后应刷新账号池");

  let asked = "";
  const originalConfirm = f.window.confirm;
  f.window.confirm = function (msg) { asked = msg; return true; };
  await f.review.accountRowActions({target: {closest: () => ({getAttribute: () => "workbuddy9.json"})}});
  f.window.confirm = originalConfirm;
  assert.match(asked, /workbuddy9\.json/, "删除前必须确认并指名凭据");
  assert(calls.indexOf("POST /admin/api/account-delete") > -1, "确认后必须提交删除请求");

  const deletesBefore = calls.filter(c => c === "POST /admin/api/account-delete").length;
  f.window.confirm = function () { return false; };
  await f.review.accountRowActions({target: {closest: () => ({getAttribute: () => "workbuddy9.json"})}});
  f.window.confirm = originalConfirm;
  assert.equal(calls.filter(c => c === "POST /admin/api/account-delete").length, deletesBefore,
    "取消确认时不能提交删除");

  const cancelsBefore = calls.filter(c => c === "POST /admin/api/account-cancel").length;
  await f.review.closeAddAccount();
  assert(f.element("addAccountPanel").classList.contains("hidden"), "关闭后表单必须隐藏");
  assert.equal(calls.filter(c => c === "POST /admin/api/account-cancel").length, cancelsBefore,
    "没有进行中的会话时关闭不应请求取消");
  audit("账号新增与删除通过", {default_name_no_path: true, confirm_required: true});
});
