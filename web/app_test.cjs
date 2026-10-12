// 使用真实前端函数和合成响应，无浏览器/第三方依赖，不调用生产接口。
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const logPath = path.join(__dirname, "..", "logs", "webui-frontend-tests-dev-" + new Date().toISOString().slice(0, 10) + ".jsonl");
function audit(event, fields = {}) {
  fs.mkdirSync(path.dirname(logPath), {recursive: true});
  fs.appendFileSync(logPath, JSON.stringify({traceId: "webui-config-tests-20261007", event, ...fields}) + "\n");
}
function response(status, body) {
  return {status, ok: status >= 200 && status < 300, json: async () => body};
}
function fixture(fetchImpl = async () => response(200, {})) {
  const elements = new Map(), session = new Map(), local = new Map(), alerts = [];
  function element(id) {
    if (!elements.has(id)) {
      const node = {innerHTML: "", textContent: "", className: "", value: "", checked: false, disabled: false,
            querySelector() { return this; }, addEventListener() {}, focus() {}};
      node.classList = {
        add(name) { node.className += " " + name; },
        remove(name) { node.className = node.className.split(/\s+/).filter(v => v !== name).join(" "); },
        toggle() {},
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
    window: {alert: message => alerts.push(message), confirm: () => true, addEventListener() {}, location: {reload() {}}},
    TextEncoder, fetch: fetchImpl, setInterval: () => 1,
  };
  let source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8");
  source = source.replace(/\}\)\(\);\s*$/, `globalThis.review = {
    state, api, renderAccounts, renderModels, renderStatus, refreshActive,
    saveKey, saveAPISettings, initializeAdmin, discoverSetup,
    showConfigWarning, loadConfigEditor, saveConfigEditor, exitConfigEdit, setTab, lockDashboard
  };})();`);
  vm.createContext(context);
  vm.runInContext(source, context);
  return {review: context.review, element, session, local, alerts, window: context.window};
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
