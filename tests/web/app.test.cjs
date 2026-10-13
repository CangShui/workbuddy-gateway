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
            querySelector() { return this; }, querySelectorAll() { return []; },
            addEventListener() {}, focus() {}};
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
    saveKey, saveAPISettings, initializeAdmin, discoverSetup,
    showConfigWarning, loadConfigEditor, saveConfigEditor, exitConfigEdit, setTab, lockDashboard,
    initListFilters, changeListFilter, reportRestoredListFilters,
    rateCell, rateKey, rateDetailHTML, ensureRateDetails, toggleRateDetail, restoreRateDetails
  };})();`);
  vm.createContext(context);
  vm.runInContext(source, context);
  return {review: context.review, element, session, local, alerts, window: context.window,
    document: context.document, cookies, cookieWrites};
}
function cells(html) {
  return [...html.matchAll(/<td(?: [^>]*)?>(.*?)<\/td>/g)].map(m => m[1]);
}

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
