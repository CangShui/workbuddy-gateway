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
function fixture(fetchImpl = async () => response(200, {}), cookies = new Map(), clock) {
  const elements = new Map(), session = new Map(), local = new Map(), alerts = [], timers = new Map();
  let timerID = 0;
  const cookieWrites = [];
  function element(id) {
    if (!elements.has(id)) {
      const node = {id, innerHTML: "", textContent: "", className: "", value: "", checked: false, disabled: false,
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
    setTimeout: (fn) => { timers.set(++timerID, fn); return timerID; },
    clearTimeout: id => timers.delete(id),
  };
  if (typeof clock === "number") {
    context.Date = class extends Date {
      constructor(...args) { super(...(args.length ? args : [clock])); }
      static now() { return clock; }
    };
  }
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
    state, api, showAlert, hideAlert, togglePolicy, renderAccounts, renderModels, renderStatus, refreshActive,
    renderPoolHealth, renderAccountAlerts, bindAccountAlertTooltip, loadAccountAlerts, lockDashboard,
    saveKey, saveAPISettings, initializeAdmin, discoverSetup,
    showConfigWarning, loadConfig, loadConfigEditor, saveConfigEditor, exitConfigEdit, setTab, lockDashboard,
    jsonHighlightHTML, renderConfigEditorHighlight,
    initListFilters, changeListFilter, reportRestoredListFilters,
    rateCell, rateKey, rateDetailHTML, ensureRateDetails, toggleRateDetail, restoreRateDetails
    , renderStats, loadStats, changeStatsFilter, loadLogs, statsLineChart, statsComboChart, statsDuration, statsNumber, statsCredit
    , openAddAccount, startAddAccount, pollAddAccount, closeAddAccount, accountRowActions
    , initStatsFilter, writeStatsFilterCookie, readStatsFilterCookie, bindStatsChartHover
    , renderRedact, loadRedact, saveRedact, previewRedact, loadRedactRecords, openRedactDetail, toggleRedactReveal, closeRedactDetail, clearRedactRecords
  };})();`);
  vm.createContext(context);
  vm.runInContext(source, context);
  return {review: context.review, element, session, local, alerts, window: context.window,
    document: context.document, cookies, cookieWrites, timers};
}
function cells(html) {
  return [...html.matchAll(/<td(?: [^>]*)?>(.*?)<\/td>/g)].map(m => m[1]);
}

test("成功提示不被轮询清掉，五秒后关闭，旧定时器不能关闭新错误", async () => {
  const f = fixture();
  f.review.state.apiKey = "synthetic-notification-key";
  f.review.showAlert("合成启停成功", true);
  const oldTimer = [...f.timers.values()][0];
  await f.review.api("/admin/api/status");
  assert.equal(f.element("alert").className, "alert ok");
  f.review.showAlert("合成错误", false, "/admin/api/account-toggle");
  oldTimer();
  assert.equal(f.element("alert").className, "alert", "迟到的成功定时器不能吞掉新错误");
  f.review.showAlert("再次成功", true);
  [...f.timers.values()][0]();
  assert.equal(f.element("alert").className, "alert hidden");
  audit("成功提示防闪烁通过", {poll_preserves_success: true, old_timer_guard: true});
});

test("无关请求不能清除错误，同一接口恢复才关闭；静默请求失败仍审计", async () => {
  let quiet = false;
  const calls = [];
  const f = fixture(async (path, options) => {
    calls.push({path, options});
    if (quiet && path === "/admin/api/stats") throw new Error("synthetic network error");
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-error-key";
  f.review.showAlert("开关保存失败", false, "/admin/api/account-toggle");
  await f.review.api("/admin/api/status");
  assert.equal(f.element("alert").textContent, "开关保存失败");
  assert.equal(f.element("alert").className, "alert");
  await f.review.api("/admin/api/account-toggle");
  assert.equal(f.element("alert").className, "alert hidden");
  quiet = true;
  await assert.rejects(f.review.api("/admin/api/stats", {quiet: true, trace: "synthetic-quiet-trace"}));
  assert.equal(f.element("alert").className, "alert hidden");
  assert(calls.some(c => c.path === "/admin/api/client-events" &&
    c.options.headers["X-Trace-ID"] === "synthetic-quiet-trace" &&
    JSON.parse(c.options.body).event === "request_failed"));
  audit("错误归属与静默请求通过", {unrelated_poll_cannot_hide_error: true, failure_audited: true});
});

test("同一账号与模型数据重复轮询不重写tbody，变化及清空仍更新", () => {
  const f = fixture();
  for (const [id, render, payload, changed] of [
    ["accountsTable", f.review.renderAccounts,
      [{path: "synthetic.json", state: "active", requests: 1}],
      [{path: "synthetic.json", state: "active", nickname: "已更新"}]],
    ["modelsTable", f.review.renderModels,
      {models: [{id: "synthetic-model", requests: 1}]},
      {models: [{id: "synthetic-model", requests: 2}]}],
  ]) {
    const tb = f.element(id);
    let html = "", writes = 0;
    Object.defineProperty(tb, "innerHTML", {configurable: true,
      get: () => html, set: value => { html = value; writes++; }});
    render(payload);
    render(payload);
    assert.equal(writes, 1, id + ": identical refresh replaced markup");
    render(changed);
    assert.equal(writes, 2, id + ": changed data was not rendered");
    render(id === "accountsTable" ? [] : {models: []});
    assert.equal(writes, 3);
    assert.match(html, /暂无/);
  }
  f.review.lockDashboard();
  assert.equal(f.element("accountsTable")._tableSourceHTML, "");
  assert.equal(f.element("modelsTable")._tableSourceHTML, "");
  audit("重复轮询跳过表格重写通过", {accounts: true, models: true, real_changes_preserved: true});
});

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
test("告警详情首次点击不被focus关闭，重复点击可关闭，不变轮询不清空详情", () => {
  const f = fixture();
  const handlers = {};
  const list = f.element("accountAlerts"), tip = f.element("accountAlertTooltip");
  list.addEventListener = (name, handler) => { handlers[name] = handler; };
  const row = {
    closest: () => row,
    getAttribute: () => "账号停用\n完整原因",
    getBoundingClientRect: () => ({left: 30, bottom: 40}),
  };
  f.window.innerWidth = 1920;
  f.window.innerHeight = 1080;
  f.review.bindAccountAlertTooltip();
  handlers.focusin({target: row});
  handlers.click({target: row});
  assert.equal(tip.hidden, false, "focus先显示不应导致首次点击立即关闭");
  assert.match(tip.textContent, /完整原因/);
  handlers.pointerleave({pointerType: "touch"});
  assert.equal(tip.hidden, false, "手指抬起的pointerleave不能关闭已打开的详情");
  handlers.click({target: row});
  assert.equal(tip.hidden, true);
  handlers.pointermove({target: row, pointerType: "mouse", clientX: 30, clientY: 40});
  handlers.click({target: row});
  assert.equal(tip.hidden, false, "鼠标先悬浮再点击也不应关闭");
  const data = {alerts: [{account: "synthetic.json", severity: "notice",
    label: "账号停用", message: '完整原因 "quotation" <tag>'}]};
  f.review.renderAccountAlerts(data);
  tip.hidden = false;
  f.review.renderAccountAlerts(data);
  assert.equal(tip.hidden, false, "相同告警轮询不应关闭悬浮详情");
  f.review.renderAccountAlerts({alerts: []});
  assert.equal(tip.hidden, true, "已恢复的告警不能留着旧详情");
  audit("告警详情交互回归通过", {first_tap_visible: true, polling_preserves_details: true});
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
  assert.equal(f.element("accountAlertsInfo").textContent, "", "来源说明文案已移除");
  f.review.renderAccountAlerts({alerts: [], missingLogCount: 1, scanTruncated: true});
  assert.match(f.element("accountAlerts").innerHTML, /暂未找到对应日志/);
  assert.match(f.element("accountAlertsInfo").textContent, /读取上限/);
  f.review.lockDashboard();
  assert.equal(f.review.state.lastAccountAlerts, null);
  assert(!f.element("accountAlerts").innerHTML.includes("dead"));
  audit("账号告警展示通过", {recovered_removed: true, escaped: true, logout_cleared: true});
});

test("告警行只保留紧凑标题，完整内容写入悬浮数据且使用告警详情标签", () => {
  const f = fixture();
  f.review.state.lastStatus = {accounts: [{path: "paused.json", state: "active", schedulingEnabled: false}]};
  f.review.renderAccountAlerts({alerts: [{account: "paused.json", state: "paused", severity: "notice",
    label: "账号停用", message: "该账号被全局调度规则停用；这不是授权失效，可在账号池恢复。", at: 1791980005}]});
  const html = f.element("accountAlerts").innerHTML;
  assert.match(html, /account-alert alert-notice/);
  assert.match(html, /data-alert-detail="[^"]*该账号被全局调度规则停用/);
  assert(!/<p>该账号被全局调度规则停用/.test(html), "告警正文不应常驻占高");
  const page = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(page.includes('<span class="badge">告警详情</span>'));
  assert(!page.includes("只读日志"));
  audit("告警紧凑悬浮通过", {compact_row: true, full_detail_in_data_attribute: true, badge_updated: true});
});

test("运行日志展示为最新行在最上方", async () => {
  const f = fixture(async url => {
    assert(url.startsWith("/admin/api/logs?lines="));
    return response(200, {lines: ["oldest", "middle", "newest"], files: [], file: "gateway.log"});
  });
  f.review.state.apiKey = "synthetic-admin";
  f.element("logLines").value = "200";
  await f.review.loadLogs(true);
  assert.equal(f.element("logBox").textContent, "newest\nmiddle\noldest");
  assert.equal(f.element("logBox").scrollTop, 0);
  audit("运行日志倒序通过", {newest_first: true});
});

test("页面移除统计落盘徽标和摘要卡四分之一圆装饰，选项卡按屏幕自适应", () => {
  const page = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  const css = fs.readFileSync(path.join(projectRoot, "web/style.css"), "utf8");
  assert(!page.includes('id="statsStorage"'));
  assert(!/\.usage-metric::after\s*\{/.test(css), "摘要卡的圆形装饰必须删除");
  assert(/\.tabs\s*\{[\s\S]*width:\s*min\(83\.3333vw,\s*3200px\)/.test(css));
  const tabCount = (page.match(/<button class="tab[" ]/g) || []).length;
  assert.equal(tabCount, 8, "总览、账号池、模型列表、脱敏、日志、配置、定时任务、访问设置");
  assert(new RegExp("\\.tabs\\s*\\{[\\s\\S]*grid-template-columns:\\s*repeat\\(" + tabCount + ",\\s*minmax\\(0,\\s*1fr\\)\\)").test(css),
    "选项卡列数必须与按钮数一致");
  assert(!new RegExp("repeat\\((?!" + tabCount + ",)\\d+,\\s*minmax\\(0,\\s*1fr\\)\\)").test(css.slice(css.indexOf(".tabs"), css.indexOf(".tab {"))));
  assert(/\.health-layout\s*\{[^}]*height:\s*196px/.test(css));
  assert(/\.account-alert\s*\{[^}]*padding:\s*3px 10px/.test(css));
  audit("布局精简与自适应通过", {stats_badge_removed: true, quarter_circle_removed: true, tabs_5_of_6_capped_4k: true});
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
  for (const i of [7, 8, 9, 12, 14]) assert.equal(row[i], "0");
  assert.match(row[11], /待签|未签/, "签到列不应再显示免费模型数量");
  assert.equal(row.length, 16, "账号表应为 16 列（已去掉刷新令牌到期）");
  const accountsPage = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(!accountsPage.includes("刷新令牌到期"), "表头不应保留已删除的列");
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

test("账号池签到列展示北京时间当天状态，移除免费模型列并转义错误提示", () => {
  const clock = Date.parse("2026-10-25T08:20:00+08:00");
  const f = fixture(undefined, new Map(), clock);
  const account = {path: "synthetic.json", state: "active", freeModels: 987654};
  for (const [status, text] of [["pending", "待签"], ["running", "待签"], ["success", "已签"],
    ["failed", "失败"], ["missed", "未签"], ["unknown", "未知"]]) {
    f.review.renderAccounts([{...account, checkin: {date: "2026-10-25", status,
      message: '<img src=x onerror="bad">', nextAt: Date.parse("2026-10-25T09:45:00+08:00") / 1000}}]);
    const row = cells(f.element("accountsTable").innerHTML);
    assert.match(row[11], new RegExp(text));
    assert(!f.element("accountsTable").innerHTML.includes("987654"));
    assert(row[11].includes("&lt;img") && !row[11].includes("<img"));
    assert(row[11].includes("北京时间 2026-10-25"));
  }
  f.review.renderAccounts([{...account, edition: "intl", checkin: {date: "2026-10-25", status: "pending"}}]);
  assert.match(cells(f.element("accountsTable").innerHTML)[11], />—</);
  f.review.renderAccounts([{...account, checkin: {date: "2026-10-25", status: "unsupported"}}]);
  assert.match(cells(f.element("accountsTable").innerHTML)[11], />—</);
  const html = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(html.includes("<th>签到</th>") && !html.includes("<th>免费模型</th>"));
  audit("账号池签到列通过", {beijing_day: true, statuses: 6, private_errors_escaped: true, free_model_column_removed: true});
});

test("北京时间跨日后不沿用昨日已签；错过今日窗口显示未签而非已签或失败", () => {
  for (const [clock, text] of [["2026-10-25T00:01:00+08:00", "待签"], ["2026-10-25T09:31:00+08:00", "未签"]]) {
    const f = fixture(undefined, new Map(), Date.parse(clock));
    f.review.renderAccounts([{path: "synthetic.json", state: "active",
      checkin: {date: "2026-10-24", status: "success", nextAt: 1900000000}}]);
    const cell = cells(f.element("accountsTable").innerHTML)[11];
    assert.match(cell, new RegExp(text));
    assert(!cell.includes("已签") && !cell.includes(" · 计划 "));
  }
  audit("签到北京时间跨日通过", {old_success_cleared: true, missed_window_not_a_failure: true});
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
    if (route === "/admin/api/config") return response(200, {exists: true, content: '{\n  "gateway": {"adminKey": "[REDACTED]", "apiPort": 8317},\n  "identity": {"responseRestore": false}\n}'});
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

test("配置预览和编辑器保留原文并高亮JSON，HTML字符被转义", async () => {
  const f = configFixture();
  const page = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(page.includes('id="configEditorHighlight"'), "编辑器缺少高亮遮罩层");
  assert(page.includes('wrap="off"'), "JSON 编辑器应关闭自动换行以保持内容与高亮对齐");
  const raw = '{\n  "gateway": {"adminKey": "<tag> & \\"quoted\\"", "apiPort": 8317},\n  "enabled": true,\n  "empty": null\n}';
  const html = f.review.jsonHighlightHTML(raw);
  assert.match(html, /json-key/);
  assert.match(html, /json-string/);
  assert.match(html, /json-number/);
  assert.match(html, /json-boolean/);
  assert.match(html, /json-null/);
  assert(html.includes("&lt;tag&gt; &amp;"), "HTML 字符必须转义");
  assert(html.includes("&quot;"), "引号必须转义");
  assert(!html.includes("<tag>"), "高亮不能把配置值拼成 HTML");
  assert.doesNotThrow(() => f.review.jsonHighlightHTML('{"unterminated": "value'));

  await f.review.loadConfig();
  assert.match(f.element("configBox").innerHTML, /json-key/);
  const editor = f.element("configEditor");
  editor.value = raw;
  f.review.renderConfigEditorHighlight();
  assert.match(f.element("configEditorHighlight").innerHTML, /json-key/);
  assert.match(f.element("configEditorHighlight").innerHTML, /json-number/);
  assert.equal(editor.value, raw, "高亮不能改动编辑内容");
  audit("配置JSON高亮通过", {preview: true, editor: true, html_escaped: true});
});

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

test("保存成功回到脱敏预览，端口热切换失败提示保留", async () => {
  const f = configFixture(async (route, options) => route === "/admin/api/config-edit" && options.method === "POST" ?
    response(200, {saved: true, adminKeyChanged: false, settings: {restartRequired: true}}) : null);
  await openConfigEditor(f);
  await f.review.saveConfigEditor();
  assert.equal(f.element("configEditor").value, "");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.review.state.configMode, "preview");
  assert.match(f.element("configResult").textContent, /监听端口尚未热切换/);
  const post = f.calls.find(c => c.route === "/admin/api/config-edit" && c.options.method === "POST");
  const body = JSON.parse(post.options.body);
  assert.equal(body.content, configContent);
  assert.equal(body.revision, "original-revision");
  assert.equal(body.acknowledged, true);
  audit("保存与端口热切换提示通过");
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
  assert.equal(calls.filter(c => c.route === "/admin/api/models?range=5h").length, 1);
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
  const missing = f.review.rateDetailHTML({rates: {}, noLogHint: "尚未找到目录或有效实测来源，不能据此判断模型没有被调用。"}, "intl", "not-logged-model");
  assert.match(missing, /暂未找到倍率来源/);
  assert.match(missing, /不能据此判断/);
  assert(!missing.includes("就会产生"));
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
  assert.match(second.panel.innerHTML, /暂未找到倍率来源/);
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

test("倍率来源独立于普通日志，展示冲突、目录刷新和有效实测而不谎报没有日志", () => {
  const f = rateFixture({rates: {}});
  const html = f.review.rateDetailHTML({
    truncated: true,
    rates: {"intl|deepseek-v4.1-flash": {
      hasSource: true, hasLog: false, source: "正常请求实测", value: "收费(倍率未知)",
      updatedAt: 1793860000, checkedAt: 1793859000, changedAt: 1793850000,
      explain: "有效实测已扣费；目录仍标免费，存在冲突", unknown: true, conflict: true,
      catalog: {model: "deepseek-v4.1-flash-sg", credits: "x0.00"},
      observation: {updatedAt: 1793860000, class: "paid", credit: 1.25},
      records: [],
    }},
  }, "intl", "deepseek-v4.1-flash-sg");
  assert.match(html, /当前倍率：收费\(倍率未知\)/);
  assert.match(html, /存在冲突/);
  assert.match(html, /目录最近成功读取/);
  assert.match(html, /价格规则最近变化/);
  assert.match(html, /usage.credit=1.25/);
  assert.match(html, /原始日志未在本次扫描窗口内找到/);
  assert(!html.includes("没有日志"));
  assert.equal(f.review.rateKey("intl", "DeepSeek-V4.1-Flash-SG"), "intl|deepseek-v4.1-flash");
  const missing = f.review.rateDetailHTML({rates: {}, truncated: true}, "cn", "m");
  assert.match(missing, /未覆盖全部记录/);
  assert(!missing.includes("请求一次"));
  const failure = f.review.rateDetailHTML({
    rates: {}, logProblem: "原始普通日志暂不可读",
  }, "cn", "m");
  assert.match(failure, /暂不可读/);
  audit("倍率持久来源、国际别名和扫描范围说明通过");
});

test("倍率来源版本改变立即失效缓存，同版本复用", async () => {
  const payload = {revision: 1, rates: {"cn|hy3": {hasSource: true, source: "目录",
    value: "0.11x", updatedAt: 1793860000, records: []}}};
  const f = rateFixture(payload);
  const row = {id: "hy3", cnMultiplier: "0.11x", intlMultiplier: "-", enabled: true};
  f.review.renderModels({models: [row], rateRevision: 1});
  await f.review.ensureRateDetails();
  assert.equal(f.review.state.rateDetails, payload);
  f.review.renderModels({models: [row], rateRevision: 1});
  assert.equal(f.review.state.rateDetails, payload);
  f.review.renderModels({models: [row], rateRevision: 2});
  assert.equal(f.review.state.rateDetails, null);
  audit("倍率版本更新不受原有60秒缓存限制");
});

test("倍率刷新进行中切换版本，旧结果不写入新缓存，已展开内容刷新时不闪回加载提示", async () => {
  let release;
  const f = fixture(async (route) => {
    if (route === "/admin/api/client-events") return response(204, {});
    if (route === "/admin/api/rate-detail") return new Promise(resolve => { release = resolve; });
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.rateRevision = "1:0";
  const pending = f.review.ensureRateDetails();
  f.review.state.rateRevision = "2:0";
  release(response(200, {revision: "1:0", rates: {}}));
  await pending;
  assert.equal(f.review.state.rateDetails, null);
  const g = rateFixture({rates: {"cn|hy3": {hasSource: true, value: "0.29x",
    source: "官方目录", updatedAt: 1793860000, records: []}}});
  const {button, panel} = fakeRateButton("cn", "hy3");
  panel.innerHTML = "previous evidence";
  button.setAttribute("aria-expanded", "true");
  g.review.state.openRateKeys["cn|hy3"] = true;
  g.review.restoreRateDetails({querySelectorAll: () => [button]}, true);
  assert.equal(panel.innerHTML, "previous evidence");
  await new Promise(resolve => setImmediate(resolve));
  assert.match(panel.innerHTML, /当前倍率：0.29x/);
  audit("倍率在途版本隔离与无闪烁更新通过");
});

// ---- 总览 · 用量与性能 ------------------------------------------------------

function statsBucket(label, over = {}) {
  return Object.assign({start: 0, label, requests: 0, success: 0, failed: 0, tokens: 0, inputTokens: 0,
    outputTokens: 0, cachedTokens: 0, tokenSamples: 0, credits: 0, creditSamples: 0, ttftSamples: 0,
    avgLatencyMs: null, avgTtftMs: null}, over);
}

function chartModel(html) {
  const attr = html.match(/data-chart-model="([^"]*)"/);
  assert(attr, "expected SVG chart geometry");
  return JSON.parse(attr[1].replace(/&quot;/g, '"').replace(/&#39;/g, "'")
    .replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&"));
}

function hoverChart(f, id, rect) {
  const container = f.element(id), model = chartModel(container.innerHTML), listeners = {};
  const shared = id === "usageChart" || id === "latencyChart";
  const group = shared ? f.element("statsCharts") : null;
  if (group && !group.testHoverListeners) {
    group.testHoverListeners = {};
    const add = group.addEventListener;
    group.addEventListener = (name, fn) => { group.testHoverListeners[name] = fn; add.call(group, name, fn); };
  }
  const metrics = {rectReads: 0, attributeWrites: 0, tooltipWrites: 0, sizeReads: 0};
  const cross = {attributes: {}, setAttribute(k, v) { metrics.attributeWrites++; this.attributes[k] = v; }};
  const dots = model.buckets[0].lines.map(() => ({attributes: {},
    setAttribute(k, v) { metrics.attributeWrites++; this.attributes[k] = v; }}));
  let html = "";
  const tooltip = {hidden: true, style: {},
    get innerHTML() { return html; }, set innerHTML(value) { metrics.tooltipWrites++; html = value; },
    get offsetWidth() { metrics.sizeReads++; return 210; }, get offsetHeight() { metrics.sizeReads++; return 120; }};
  const area = {addEventListener(name, fn) { listeners[name] = fn; }};
  const svg = {getAttribute: () => JSON.stringify(model),
    querySelector: selector => selector === ".chart-hover-area" ? area : cross,
    querySelectorAll: () => dots, getBoundingClientRect: () => { metrics.rectReads++; return rect; }};
  container.querySelector = selector => selector === "svg" ? svg : tooltip;
  container.clientWidth = rect.width;
  container.clientHeight = rect.height;
  f.review.bindStatsChartHover(container);
  // The real shared-chart listener is delegated to the parent, so emulate
  // bubbling in this DOM stub rather than calling an obsolete plot listener.
  if (shared) {
    listeners.pointermove = event => group.testHoverListeners.pointermove(event);
    listeners.pointerleave = () => {};
    listeners.pointercancel = event => group.testHoverListeners.pointercancel(event);
  }
  return {model, listeners, tooltip, cross, dots, metrics,
    move(index, y) {
      listeners.pointermove({clientX: rect.left + model.buckets[index].x * rect.width / model.width,
        clientY: rect.top + (y === undefined ? rect.height / 2 : y)});
    }};
}

function animationClock(f, cancellable = true) {
  let id = 0;
  const callbacks = new Map();
  f.window.requestAnimationFrame = callback => { callbacks.set(++id, callback); return id; };
  if (cancellable) f.window.cancelAnimationFrame = frame => callbacks.delete(frame);
  return {get pending() { return callbacks.size; }, flush() {
    const queued = [...callbacks.values()];
    callbacks.clear();
    queued.forEach(callback => callback());
  }};
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

test("统计轮询仅服务器时钟变化时复用图表，跨未来桶边界、数据及宽度变化仍重绘", () => {
  const f = fixture(), chart = f.element("usageChart");
  let markup = "", writes = 0;
  Object.defineProperty(chart, "innerHTML", {configurable: true,
    get: () => markup, set: value => { markup = value; writes++; }});
  const start = Date.parse("2026-10-27T08:00:00+08:00");
  const data = statsPayload({serverTime: start + 1000, buckets: [
    statsBucket("08:00", {start, requests: 1, tokens: 100, tokenSamples: 1}),
    statsBucket("09:00", {start: start + 3600000}),
  ]});
  f.review.renderStats(data);
  f.review.renderStats({...data, serverTime: start + 2000});
  assert.equal(writes, 1, "服务器时间每秒推进不应清空悬浮并重建SVG");
  f.review.renderStats({...data, serverTime: start + 2000,
    storage: {lastSavedAt: start + 2000, message: "合成存储警告"}});
  assert.equal(writes, 1, "落盘时间变化不能重建图表");
  assert.equal(f.element("statsInfo").textContent, "合成存储警告");
  f.review.renderStats({...data, serverTime: start + 3600001});
  assert.equal(writes, 2, "原未来桶到时必须重新显示，不能被缓存冻结");
  chart.clientWidth = 1200;
  f.element("latencyChart").clientWidth = 1200;
  f.review.renderStats({...data, serverTime: start + 3600001});
  assert.equal(writes, 3, "窗口改变必须更新viewBox");
  const changed = {...data, serverTime: start + 3600001,
    summary: {...data.summary, tokens: 1000}};
  f.review.renderStats(changed);
  assert.equal(writes, 4, "真实统计变化不能被缓存吞掉");
  f.review.lockDashboard();
  f.review.renderStats(changed);
  assert.equal(writes, 5, "退出登录清空后必须允许重新渲染");
  audit("统计相同数据复用通过", {clock_only_skipped: true, future_boundary_redraw: true,
    width_and_data_redraw: true, logout_resets_cache: true});
});

test("后台读取统计不插入临时提示，不覆盖正在显示的存储警告", async () => {
  let finish;
  const payload = statsPayload({storage: {mode: "memory", message: "统计暂时无法写入，保留内存数据"}});
  const f = fixture(async route => route.startsWith("/admin/api/stats?") ?
    new Promise(resolve => { finish = () => resolve(response(200, payload)); }) : response(204, {}));
  f.review.state.apiKey = "synthetic-stats-key";
  f.review.renderStats(payload);
  const before = f.element("statsInfo").textContent;
  const loading = f.review.loadStats(false);
  assert.equal(f.element("statsInfo").textContent, before);
  finish();
  await loading;
  assert.equal(f.element("statsInfo").textContent, before);
  audit("统计静默刷新保持布局通过", {temporary_loading_row_removed: true, storage_warning_preserved: true});
});

test("TTFT新旧口径隔离：旧快照不伪装成首Token，明确显示排除数", () => {
  const f=fixture();
  const old=statsPayload({ttftMetric: undefined});
  old.summary.avgTtftMs=87190;
  f.review.renderStats(old);
  assert(!f.element("usageSummary").innerHTML.includes("87.19"));
  assert.match(f.element("usageSummary").innerHTML,/平均首Token/);
  assert.match(f.element("latencyLegend").innerHTML, /deepseek-v4\.1-flash/);
  const oldReadout = chartModel(f.element("latencyChart").innerHTML).buckets[0].readout;
  assert.equal(oldReadout.find(r => r.label === "平均首Token").display, "—", "共享浮层也不能混入旧TTFT口径");
  const current=statsPayload();
  current.summary.legacyTtftSamples=12;
  f.review.renderStats(current);
  assert.equal(f.element("statsInfo").textContent, "", "统计口径长文案已移除");
  assert.match(f.element("usageSummary").innerHTML,/0\.30 s/);
  f.review.renderModels({models:[{id:"legacy",avgTtftMs:87190,hasTtft:true}]});
  assert(!f.element("modelsTable").innerHTML.includes("87.19"));
  audit("首Token新旧样本隔离通过",{legacy_samples_excluded:true});
});

test("统计摘要区分未知与零值，落盘徽标已移除但仍提示内存异常", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const cards = f.element("usageSummary").innerHTML;
  assert.match(cards, /总消耗 Token/);
  assert.match(cards, /1\.25/, "积分必须按实际 usage.credit 展示");
  assert.match(cards, /1\/2 请求有积分/, "必须暴露积分样本覆盖数");
  // 无样本时显示未知，而不是 0。
  f.review.renderStats(statsPayload({summary: {requests: 1, success: 1, failed: 0, tokenSamples: 0,
    creditSamples: 0, ttftSamples: 0, avgLatencyMs: 900, avgTtftMs: null}}));
  const unknown = f.element("usageSummary").innerHTML;
  assert.match(unknown, /总消耗 Token<\/div><div class="usage-value">—</, "无 Token 样本不得显示为 0");
  assert.match(unknown, /总消耗积分<\/div><div class="usage-value">—</, "无积分样本不得显示为 0");
  // 仅内存模式必须给出明确提示，避免被当成已持久化。
  f.review.renderStats(statsPayload({storage: {mode: "memory", message: "统计暂时无法写入，仍在内存统计"}}));
  assert.equal(f.element("statsStorage").textContent, "", "统计落盘徽标块已删除");
  assert.match(f.element("statsInfo").textContent, /统计暂时无法写入/);
  audit("统计摘要与落盘状态展示通过", {unknown_not_zero: true, memory_mode_flagged: true});
});

test("耗时折线跨过空桶连接相邻样本，不伪造零值", () => {
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
  // 中间桶无样本 ⇒ 仍是一条连续折线，但空桶不画点、不伪造 0ms。
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line series-first"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line/g) || []).length, 2, "空桶应跨过连接，不能断开");
  assert.equal((svg.match(/<circle class="series-token"/g) || []).length, 2, "空桶不能伪造折线点");
  assert.match(svg, /data-chart-model=/, "悬浮层必须带几何数据");
  assert.equal(f.review.statsLineChart([statsBucket("2026-10-14")], [
    {label: "平均总耗时", color: "token", value: b => b.avgLatencyMs},
  ], "耗时", String), '<p class="stats-empty">该时间范围暂无可用样本</p>', "全空必须显示空状态");
  audit("耗时折线跨空桶与空状态通过", {no_fake_zero: true, connected_paths: 2});
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
  assert(svg.includes("deepseek-v4.1-flash"), "模型名必须进入悬浮数据");
  assert(svg.includes("1.50 s"), "耗时值必须进入悬浮数据");
  assert(svg.includes("hy3") && svg.includes("0.80 s"));
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

test("双轴组合图：折线走左轴、柱形走右轴，缺样本处不补柱", () => {
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
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 1, "折线应跨过空桶连接");
  assert.equal((svg.match(/class="chart-bar series-credit"/g) || []).length, 2, "中间桶缺样本不画柱");
  assert.match(svg, /class="chart-crosshair"/, "必须有十字线");
  assert.match(svg, /class="chart-hover-area"/, "必须有鼠标命中层");
  assert.match(svg, /class="chart-tooltip"/, "必须有悬浮读数层");
  assert(!f.review.statsComboChart([statsBucket("2026-10-14")], {
    lines: [], bars: [{label: "积分", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "空", leftFormat: String, box: {w: 600, h: 240}
  }).includes("<svg"), "全空必须显示空状态");
  audit("双轴组合图通过", {gap_connected: true, dual_axis: true});
});

test("用量折线把无样本时间桶按 0 连接并生成悬浮数据", () => {
  const f = fixture();
  const svg = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 100, tokenSamples: 1}),
    statsBucket("2026-10-14"),
    statsBucket("2026-10-15", {tokens: 300, tokenSamples: 1}),
  ], {
    lines: [{label: "总 Token", color: "token", connectZero: true, value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [], title: "零值连接", leftFormat: String, box: {w: 600, h: 240}
  });
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 1);
  assert.equal((svg.match(/<circle class="series-token"/g) || []).length, 3, "空桶必须补 0 点");
  assert.match(svg, /class="chart-tooltip"/, "悬浮层必须存在");
  assert.match(svg, /data-chart-model=/, "悬浮层必须带逐桶日期与数值");
  assert(svg.includes("2026-10-14") && svg.includes('&quot;display&quot;:&quot;0&quot;'), "空桶必须按 0 进入悬浮数据");
  audit("零值连接与悬浮层通过", {zero_points: 3, tooltip: true});
});

test("图表悬浮层按鼠标位置显示日期和逐系列读数", () => {
  const f = fixture();
  const svgText = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 100, tokenSamples: 1, credits: 2, creditSamples: 1}),
    statsBucket("2026-10-14"),
    statsBucket("2026-10-15", {tokens: 300, tokenSamples: 1, credits: 4, creditSamples: 1}),
  ], {
    lines: [{label: "总 Token", color: "token", connectZero: true, value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [{label: "积分", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "悬浮", leftFormat: String, rightFormat: String, box: {w: 600, h: 240}
  });
  const encoded = svgText.match(/data-chart-model="([^"]*)"/)[1]
    .replace(/&quot;/g, '"').replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&#39;/g, "'");
  const model = JSON.parse(encoded);
  const listeners = {};
  const cross = {attributes: {}, setAttribute(k, v) { this.attributes[k] = v; }};
  const dots = model.buckets[0].lines.map(() => ({attributes: {}, setAttribute(k, v) { this.attributes[k] = v; }}));
  const tooltip = {hidden: true, innerHTML: "", style: {}, offsetWidth: 180, offsetHeight: 80};
  const area = {addEventListener(name, fn) { listeners[name] = fn; }};
  const svg = {
    getAttribute: () => JSON.stringify(model),
    querySelector: selector => selector === ".chart-hover-area" ? area : cross,
    querySelectorAll: () => dots,
    getBoundingClientRect: () => ({left: 0, top: 0, width: 600, height: 240}),
  };
  const container = {querySelector: selector => selector === "svg" ? svg : tooltip, clientWidth: 600, clientHeight: 240};
  f.review.bindStatsChartHover(container);
  listeners.pointermove({clientX: 357});
  assert.equal(tooltip.hidden, false);
  assert.match(tooltip.innerHTML, /2026-10-14/);
  assert.match(tooltip.innerHTML, /总 Token/);
  assert.match(tooltip.innerHTML, />0</);
  assert.match(tooltip.innerHTML, /积分/);
  assert.equal(cross.attributes.visibility, "visible");
  assert.equal(dots[0].attributes.visibility, "visible");
  listeners.pointerleave();
  assert.equal(tooltip.hidden, true);
  assert.equal(cross.attributes.visibility, "hidden");
  audit("鼠标十字线与逐系列读数通过", {crosshair: true, tooltip: true});
});

test("两张统计图共享绘图区、日期刻度和四项汇总读数，含窄屏与双轴量级差异", () => {
  for (const width of [260, 380, 900, 1800]) {
    const f = fixture();
    f.element("usageChart").clientWidth = width;
    f.element("latencyChart").clientWidth = width;
    f.review.renderStats(statsPayload());
    const usage = chartModel(f.element("usageChart").innerHTML);
    const latency = chartModel(f.element("latencyChart").innerHTML);
    assert.equal(usage.width, latency.width);
    assert.equal(usage.left, latency.left);
    assert.equal(usage.plotW, latency.plotW);
    assert(usage.plotW >= 98, "small screens retain a usable plot");
    assert.deepEqual(usage.buckets.map(r => r.x), latency.buckets.map(r => r.x));
    for (let i = 0; i < usage.buckets.length; i++) {
      const row = usage.buckets[i];
      assert.deepEqual(row.readout.map(v => v.label), ["总 Token", "积分", "平均总耗时", "平均首Token"]);
      assert.deepEqual(row.readout, latency.buckets[i].readout);
    }
    assert.deepEqual(usage.buckets[0].readout.map(r => r.display), ["300", "1.25", "1.20 s", "0.30 s"]);
    const xLabels = html => [...html.matchAll(/<text class="chart-label" text-anchor="[^"]+" x="([^"]+)" y="[^"]+">([^<]+)<\/text>/g)]
      .filter(m => /10-1[34]/.test(m[2])).map(m => [m[1], m[2]]);
    assert.deepEqual(xLabels(f.element("usageChart").innerHTML), xLabels(f.element("latencyChart").innerHTML));
  }
  audit("两图共享时间轴和四项读数通过", {responsive_widths: 4, aligned: true, readout_count: 4});
});

test("共享四项读数区分空闲桶和缺失usage，不更改原始统计", () => {
  const f = fixture(), payload = statsPayload();
  payload.buckets.push(statsBucket("2026-10-15", {requests: 1, avgLatencyMs: 900}));
  const original = JSON.stringify(payload);
  f.review.renderStats(payload);
  const buckets = chartModel(f.element("usageChart").innerHTML).buckets;
  assert.deepEqual(buckets[1].readout.map(r => r.display), ["0", "0", "—", "—"]);
  assert.deepEqual(buckets[2].readout.map(r => r.display), ["—", "—", "0.90 s", "—"]);
  assert.equal(JSON.stringify(payload), original);
  audit("共享读数空样本口径通过", {idle_zero: true, unknown_usage_preserved: true});
});

test("未来时间桶不补零、不画点线柱；保留过去零值和两图对齐并使用服务端时间", () => {
  const f = fixture(), now = Date.parse("2026-10-24T11:30:00+08:00");
  const payload = statsPayload({serverTime: now});
  payload.buckets = ["2026-10-24T09:00:00+08:00", "2026-10-24T10:00:00+08:00",
    "2026-10-24T11:00:00+08:00", "2026-10-24T12:00:00+08:00", "2026-10-25T00:00:00+08:00"]
    .map((at, i) => statsBucket(at, {start: Date.parse(at), ...(i === 0 || i >= 3
      ? {tokens: i ? 1e12 : 300, tokenSamples: 1, credits: i ? 1e9 : 2, creditSamples: 1}
      : {})}));
  payload.modelLatency = [{model: "synthetic-model", avgLatencyMs: [1000, null, 1200, 1e12, 1e12],
    avgTtftMs: [100, null, 120, 1e12, 1e12]}];
  const before = JSON.stringify(payload);
  f.review.renderStats(payload);
  const usage = chartModel(f.element("usageChart").innerHTML);
  const latency = chartModel(f.element("latencyChart").innerHTML);
  assert.deepEqual(usage.buckets.map(b => b.x), latency.buckets.map(b => b.x));
  assert.equal(usage.buckets.length, 5, "future buckets must not be filtered out and shift model indices");
  assert.deepEqual(usage.buckets.map(b => b.lines[0].value), [300, 0, 0, null, null]);
  assert.deepEqual(latency.buckets.map(b => b.lines[0].value), [1000, null, 1200, null, null]);
  for (const model of [usage, latency]) {
    assert.deepEqual(model.buckets.map(b => b.future), [false, false, false, true, true]);
    assert(model.buckets.slice(3).every(b => b.lines.concat(b.bars).every(v => v.value === null && v.y === null)));
    assert(model.buckets.slice(3).every(b => b.readout.length === 4 && b.readout.every(r => r.display === "—")));
  }
  assert.equal((f.element("usageChart").innerHTML.match(/<circle class="series-token"/g) || []).length, 3);
  assert.equal((f.element("usageChart").innerHTML.match(/class="chart-bar series-credit"/g) || []).length, 1);
  assert(!f.element("usageChart").innerHTML.includes("1000000.00M"), "future values must not inflate the scale");
  f.element("statsCharts").getBoundingClientRect = () => ({left: 0, top: 0, width: 900, height: 700});
  const usageHover = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
  usageHover.move(3);
  assert.match(usageHover.tooltip.innerHTML, /未到该时间/);
  assert.equal((usageHover.tooltip.innerHTML.match(/class="chart-tooltip-row"/g) || []).length, 4);
  assert.equal(usageHover.dots[0].attributes.visibility, "hidden");
  assert.equal(JSON.stringify(payload), before);
  audit("未来时间桶留白通过", {server_clock: true, future_buckets: 2, past_zero_connected: true, aligned: true});
});

test("未来时间判定按绝对时间支持所有粒度，当前桶开始边界仍绘制", () => {
  const f = fixture(), now = Date.parse("2030-01-01T00:00:00+08:00");
  for (const period of ["hour", "day", "week", "month"]) {
    const buckets = [now - 1, now, now + 1].map(start => statsBucket(period, {start, tokens: 7, credits: 3}));
    const html = f.review.statsComboChart(buckets, {
      nowMs: now, lines: [{label: "Token", color: "token", connectZero: true, value: b => b.tokens}],
      bars: [{label: "积分", color: "credit", value: b => b.credits}], rightFormat: String
    });
    const rows = chartModel(html).buckets;
    assert.deepEqual(rows.map(b => b.future), [false, false, true]);
    assert.deepEqual(rows.map(b => b.lines[0].value), [7, 7, null]);
    assert.deepEqual(rows.map(b => b.bars[0].value), [3, 3, null]);
  }
  const empty = f.review.statsComboChart([statsBucket("future", {start: now + 1})], {
    nowMs: now, lines: [{label: "Token", connectZero: true, value: () => null}]
  });
  assert.match(empty, /所选时间范围尚未开始/);
  assert(!empty.includes("<svg"), "all-future range must not masquerade as measured zero");
  audit("未来时间桶边界通过", {granularities: 4, current_bucket_included: true, all_future_empty: true});
});

test("旧统计接口缺少serverTime时回退本机时间，无效桶时间不影响旧数据展示", () => {
  const f = fixture(), now = Date.now();
  const html = f.review.statsComboChart([
    statsBucket("old", {start: 0}), statsBucket("invalid", {start: "invalid"}),
    statsBucket("future", {start: now + 86400000})
  ], {lines: [{label: "Token", connectZero: true, value: () => null}]});
  assert.deepEqual(chartModel(html).buckets.map(b => b.lines[0].value), [0, 0, null]);
  audit("旧接口兼容通过", {fallback_clock: true, invalid_time_safe: true});
});

test("悬停任一图都显示四项读数，虚线跨卡片间隔且只记录一次交互起止", () => {
  const calls = [];
  const f = fixture(async (route, options) => { calls.push({route, options}); return response(204, {}); });
  f.review.state.apiKey = "synthetic-private-hover-key";
  f.element("usageChart").clientWidth = 900;
  f.element("latencyChart").clientWidth = 900;
  f.review.renderStats(statsPayload());
  const groupEvents = {}, group = f.element("statsCharts"), guide = f.element("statsChartsCrosshair");
  group.getBoundingClientRect = () => ({left: 20, top: 100, width: 900, height: 750});
  group.addEventListener = (name, callback) => { groupEvents[name] = callback; };
  const usage = hoverChart(f, "usageChart", {left: 20, top: 130, width: 900, height: 280});
  const latency = hoverChart(f, "latencyChart", {left: 20, top: 520, width: 900, height: 280});
  usage.move(0);
  assert.equal(guide.hidden, false);
  assert.equal((usage.tooltip.innerHTML.match(/class="chart-tooltip-row"/g) || []).length, 4);
  assert.match(usage.tooltip.innerHTML, /平均总耗时/);
  assert.match(usage.tooltip.innerHTML, /平均首Token/);
  assert.equal(usage.cross.attributes.visibility, "hidden", "one guide should span both cards");
  assert.equal(latency.dots[0].attributes.visibility, "visible", "other chart highlights same bucket");
  assert(Number.parseFloat(guide.style.height) > 390, "guide spans both plots and the inter-card gap");
  const position = usage.tooltip.style.transform;
  usage.move(0, 170);
  assert.notEqual(usage.tooltip.style.transform, position, "tooltip follows pointer even within the same bucket");
  usage.listeners.pointerleave();
  assert.equal(guide.hidden, false, "crossing the gap must not break the guide");
  latency.move(0);
  assert.equal(usage.tooltip.hidden, true, "only the active pane has a tooltip");
  assert.equal(latency.tooltip.hidden, false);
  assert.equal((latency.tooltip.innerHTML.match(/class="chart-tooltip-row"/g) || []).length, 4);
  assert.equal(calls.length, 1, "mousemove must not spam audit requests");
  groupEvents.pointerleave();
  assert.equal(guide.hidden, true);
  assert.equal(latency.tooltip.hidden, true);
  assert.equal(calls.length, 2);
  assert.deepEqual(calls.map(c => JSON.parse(c.options.body).event), ["stats_chart_hover_started", "stats_chart_hover_ended"]);
  assert.equal(calls[0].options.headers["X-Trace-ID"], calls[1].options.headers["X-Trace-ID"]);
  assert(calls.every(c => !c.options.body.includes("synthetic-private-hover-key") && !c.options.body.includes("300")));
  audit("跨图游标联动与落盘审计通过", {spans_gap: true, one_tooltip: true, trace_reused: true});
});

test("快速移动合并为每帧一次；同桶只移动浮层、不重建内容或重复测量", () => {
  const calls = [], f = fixture(async (route, options) => { calls.push({route, options}); return response(204, {}); });
  const clock = animationClock(f);
  f.review.state.apiKey = "synthetic-performance-key";
  f.review.renderStats(statsPayload());
  let groupReads = 0;
  f.element("statsCharts").getBoundingClientRect = () => {
    groupReads++;
    return {left: 20, top: 100, width: 900, height: 750};
  };
  const usage = hoverChart(f, "usageChart", {left: 20, top: 130, width: 900, height: 280});
  const latency = hoverChart(f, "latencyChart", {left: 20, top: 520, width: 900, height: 280});
  for (let i = 0; i < 1000; i++) usage.move(i % 2, 80 + i % 80);
  assert.equal(clock.pending, 1);
  assert.equal(usage.metrics.rectReads, 0);
  assert.equal(usage.metrics.tooltipWrites, 0);
  clock.flush();
  assert.equal(usage.metrics.rectReads, 1);
  assert.equal(latency.metrics.rectReads, 1);
  assert.equal(groupReads, 1);
  assert.equal(usage.metrics.tooltipWrites, 1);
  assert.equal(usage.metrics.sizeReads, 2);
  assert.equal(latency.metrics.tooltipWrites, 0);
  assert.equal(latency.metrics.sizeReads, 0, "inactive pane never measures its hidden tooltip");
  assert.match(usage.tooltip.innerHTML, /2026-10-14/, "newest pointer wins");
  const before = {...usage.metrics}, otherBefore = {...latency.metrics}, position = usage.tooltip.style.transform;
  for (let i = 0; i < 500; i++) usage.move(1, 190);
  clock.flush();
  assert.equal(usage.metrics.tooltipWrites, before.tooltipWrites);
  assert.equal(usage.metrics.sizeReads, before.sizeReads);
  assert.equal(usage.metrics.attributeWrites, before.attributeWrites);
  assert.equal(latency.metrics.attributeWrites, otherBefore.attributeWrites);
  assert.notEqual(usage.tooltip.style.transform, position);
  assert.equal(calls.length, 1, "audit still records one start, not 1500 pointer events");
  usage.move(0);
  latency.move(1);
  assert.equal(clock.pending, 1, "both panes share a single animation queue");
  clock.flush();
  assert.equal(usage.tooltip.hidden, true);
  assert.equal(latency.tooltip.hidden, false, "latest pane wins too");
  assert.match(latency.tooltip.innerHTML, /2026-10-14/);
  latency.listeners.pointercancel();
  assert.equal(calls.length, 2);
  assert.equal(calls[0].options.headers["X-Trace-ID"], calls[1].options.headers["X-Trace-ID"]);
  audit("悬浮按帧合并与缓存通过", {pointer_events: 1500, painted_frames: 2, same_bucket_dom_rebuilds: 0,
    inactive_tooltip_reads: 0, shared_queue: true, audit_events: 2});
});

test("双图区父级覆盖横轴、标题、卡片留白和两图间隙，仍只每帧读取一次布局", () => {
  const f = fixture(), clock = animationClock(f);
  f.review.renderStats(statsPayload());
  const group = f.element("statsCharts");
  let groupReads = 0;
  group.getBoundingClientRect = () => { groupReads++; return {left: 20, top: 100, width: 900, height: 750}; };
  const usage = hoverChart(f, "usageChart", {left: 20, top: 130, width: 900, height: 280});
  const latency = hoverChart(f, "latencyChart", {left: 20, top: 520, width: 900, height: 280});
  for (const [region, y, active] of [
    ["upper-title", 105, usage], ["upper-axis", 400, usage], ["upper-padding", 420, usage],
    ["gap-near-upper", 450, usage], ["gap-near-lower", 490, latency],
    ["lower-title", 510, latency], ["lower-axis", 790, latency], ["lower-padding", 820, latency]
  ]) {
    const before = {usage: usage.metrics.rectReads, latency: latency.metrics.rectReads, group: groupReads};
    for (let i = 0; i < 60; i++) {
      group.testHoverListeners.pointermove({clientX: 20 + usage.model.buckets[i % 2].x, clientY: y,
        target: {tagName: "DIV", textContent: region}});
    }
    assert.equal(clock.pending, 1, region);
    assert.equal(usage.metrics.rectReads, before.usage, "physical event handler must not read layout");
    clock.flush();
    assert.equal(usage.metrics.rectReads - before.usage, 1, region);
    assert.equal(latency.metrics.rectReads - before.latency, 1, region);
    assert.equal(groupReads - before.group, 1, region);
    assert.equal(active.tooltip.hidden, false, region);
    assert.equal((active === usage ? latency : usage).tooltip.hidden, true, region);
    assert.match(active.tooltip.innerHTML, /2026-10-14/, region);
    assert.equal(f.element("statsChartsCrosshair").hidden, false, region);
  }
  // The shared group listener stays single even after rebinding the second pane.
  assert.deepEqual(Object.keys(group.testHoverListeners).sort(), ["pointercancel", "pointerleave", "pointermove"]);
  group.testHoverListeners.pointerleave();
  assert.equal(usage.tooltip.hidden, true);
  assert.equal(latency.tooltip.hidden, true);
  audit("录屏边缘路径覆盖通过", {regions: 8, events_per_region: 60, layout_reads_per_pane_per_frame: 1,
    nearest_pane_tooltip: true, no_interaction_dead_zones: true});
});

test("离开图表、取消、重绘或锁定页面后，已排队的悬浮不会复活", () => {
  for (const cancellable of [true, false]) {
    for (const action of ["leave", "cancel", "render", "lock", "tab"]) {
      const calls = [], f = fixture(async (route, options) => { calls.push({route, options}); return response(204, {}); });
      const clock = animationClock(f, cancellable);
      f.review.state.apiKey = "synthetic-cancel-key";
      f.review.renderStats(statsPayload());
      const groupEvents = {};
      f.element("statsCharts").addEventListener = (event, fn) => { groupEvents[event] = fn; };
      f.element("statsCharts").getBoundingClientRect = () => ({left: 0, top: 0, width: 900, height: 700});
      const usage = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
      usage.move(0);
      if (action === "leave") groupEvents.pointerleave();
      if (action === "cancel") usage.listeners.pointercancel();
      // Identical polls deliberately no longer redraw; exercise a real update.
      if (action === "render") f.review.renderStats(statsPayload({startedAt: 1760000000001}));
      if (action === "lock") f.review.lockDashboard();
      if (action === "tab") f.review.setTab("accounts");
      clock.flush();
      assert.equal(usage.tooltip.hidden, true, `${action}: stale callback must not restore tooltip`);
      assert.equal(usage.metrics.tooltipWrites, 0);
      assert.equal(f.element("statsChartsCrosshair").hidden, true);
      assert(!calls.some(c => JSON.parse(c.options.body || "{}").event === "stats_chart_hover_started"));
    }
  }
  audit("悬浮待绘制帧取消通过", {lifecycle_actions: 5, cancel_api_optional: true, stale_hover_rejected: true});
});

test("旧帧未能物理取消时，重绘后仍只处理新图的新位置", () => {
  const f = fixture(), clock = animationClock(f, false);
  f.review.renderStats(statsPayload());
  f.element("statsCharts").getBoundingClientRect = () => ({left: 0, top: 0, width: 900, height: 700});
  const oldChart = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
  oldChart.move(0);
  // Replacing innerHTML removes the old SVG in a browser; mirror that in the
  // stub so the redraw cannot accidentally rebind the detached test element.
  f.element("usageChart").querySelector = () => null;
  f.review.renderStats(statsPayload({startedAt: 1760000000001}));
  const chart = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
  chart.move(1);
  assert.equal(clock.pending, 2);
  clock.flush();
  assert.equal(oldChart.metrics.tooltipWrites, 0);
  assert.equal(chart.metrics.tooltipWrites, 1);
  assert.match(chart.tooltip.innerHTML, /2026-10-14/);
  assert.equal(chart.tooltip.hidden, false);
  audit("重绘后旧帧不干扰新帧通过", {generation_guard: true, latest_position_preserved: true});
});

test("独立图表悬浮也按帧合并，并在离开或取消后清除未绘制事件", () => {
  const f = fixture(), clock = animationClock(f);
  f.element("standaloneChart").innerHTML = f.review.statsComboChart(statsPayload().buckets, {
    lines: [{label: "Token", color: "token", connectZero: true, value: b => b.tokens}]
  });
  const chart = hoverChart(f, "standaloneChart", {left: 0, top: 0, width: 900, height: 280});
  chart.move(0);
  chart.listeners.pointerleave();
  clock.flush();
  assert.equal(chart.tooltip.hidden, true);
  chart.move(1);
  clock.flush();
  assert.equal(chart.tooltip.hidden, false);
  assert.match(chart.tooltip.innerHTML, /2026-10-14/);
  assert.equal(chart.cross.attributes.visibility, "visible");
  chart.move(0);
  chart.listeners.pointercancel();
  clock.flush();
  assert.equal(chart.tooltip.hidden, true);
  assert.equal(chart.cross.attributes.visibility, "hidden");
  audit("独立图表按帧交互通过", {cancel_on_leave: true, cancel_on_pointercancel: true});
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

test("统计筛选记进 Cookie：粒度/范围/日期可恢复，失效账号退回全局", async () => {
  const f = fixture(async () => response(200, statsPayload({account: "acc-a"})));
  f.review.state.apiKey = "synthetic-admin";

  // 用户改过粒度、范围与日期后查询，四项都应写进 Cookie。
  f.element("statsPeriod").value = "week";
  f.element("statsStart").value = "2026-10-01";
  f.element("statsEnd").value = "2026-10-07";
  f.review.state.statsDatesTouched = true;
  f.element("statsAccount").value = "acc-a";
  await f.review.changeStatsFilter(false);
  const saved = JSON.parse(decodeURIComponent(f.cookies.get("gateway_stats_filter")));
  assert.equal(saved.period, "week");
  assert.equal(saved.account, "acc-a");
  assert.equal(saved.start, "2026-10-01");
  assert.equal(saved.end, "2026-10-07");
  const stored = f.cookieWrites[f.cookieWrites.length - 1];
  assert.match(stored, /Path=\/ui/);
  assert.match(stored, /SameSite=Lax/);
  assert.match(stored, /Max-Age=31536000/);

  // 新会话带着同一个 Cookie 打开，控件自动回到上次选择。
  const restored = fixture(async () => response(200, statsPayload({account: "acc-a"})), new Map(f.cookies));
  restored.review.initStatsFilter();
  assert.equal(restored.element("statsPeriod").value, "week");
  assert.equal(restored.element("statsStart").value, "2026-10-01");
  assert.equal(restored.element("statsEnd").value, "2026-10-07");
  assert.equal(restored.review.state.statsAccount, "acc-a");

  // 没动过日期时不冻结默认区间，Cookie 里的日期保持为空。
  const plain = fixture(async () => response(200, {}), new Map());
  plain.review.state.apiKey = "synthetic-admin";
  plain.element("statsStart").value = "";
  plain.element("statsEnd").value = "";
  plain.review.state.statsDatesTouched = false;
  await plain.review.changeStatsFilter(false);
  const plainSaved = JSON.parse(decodeURIComponent(plain.cookies.get("gateway_stats_filter")));
  assert.equal(plainSaved.start, "");
  assert.equal(plainSaved.end, "");

  // 记住的账号已被删除：服务端返回 400 后自动清掉并回到全局查询。
  const seen = [];
  const staleJar = new Map([["gateway_stats_filter",
    encodeURIComponent(JSON.stringify({period: "day", account: "gone", start: "", end: ""}))]]);
  const stale = fixture(async (url) => {
    seen.push(url);
    return url.indexOf("account=gone") > -1
      ? response(400, {error: "账号统计标识不存在"})
      : response(200, statsPayload());
  }, staleJar);
  stale.review.state.apiKey = "synthetic-admin";
  stale.review.initStatsFilter();
  assert.equal(stale.review.state.statsAccount, "gone");
  await stale.review.loadStats(true);
  assert.equal(stale.review.state.statsAccount, "", "失效账号必须被清掉");
  assert(seen.some(function (u) { return u.indexOf("account=") === -1; }), "必须回退到全局查询");
  assert.equal(JSON.parse(decodeURIComponent(stale.cookies.get("gateway_stats_filter"))).account, "");

  audit("统计筛选 Cookie 通过", {restored: true, dates_opt_in: true, stale_account_fallback: true});
});

test("告警条目排成一行且不再有日志跳转；统计范围可按站点筛选；版本号可点", async () => {
  const calls = [];
  const f = fixture(async (url) => {
    calls.push(url);
    if (url.indexOf("/admin/api/stats?") === 0) return response(200, statsPayload());
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin";
  f.review.state.lastStatus = {accounts: [{path: "a.json", state: "cooldown"}]};
  f.review.renderAccountAlerts({alerts: [{account: "a.json", state: "cooldown", severity: "warning",
    label: "账号冷却", message: "等待恢复", at: 1791980005,
    logFile: "gateway-2026-10-16.log", logOffset: 12345}]});
  const alertHtml = f.element("accountAlerts").innerHTML;
  assert(!alertHtml.includes("account-alert-source"), "日志来源整块必须删除");
  assert(!alertHtml.includes("data-log-file"), "日志跳转入口必须删除");
  assert(!alertHtml.includes("linklike"), "跳转按钮样式必须删除");
  assert(/class="account-alert-head">[\s\S]*account-alert-file[\s\S]*<time>/.test(alertHtml),
    "状态、类别、凭据与时间必须排在同一行");

  f.review.renderStats(statsPayload());
  const options = f.element("statsAccount").innerHTML;
  assert(options.includes('value="site:intl"') && options.includes('value="site:cn"'),
    "统计范围必须包含国际站与国内站");
  f.element("statsAccount").value = "site:intl";
  await f.review.changeStatsFilter(false);
  const siteCall = calls.filter(function (u) { return u.indexOf("site=intl") > -1; });
  assert(siteCall.length > 0, "选中站点后必须按 site 查询");
  assert(siteCall.every(function (u) { return u.indexOf("account=") === -1; }), "站点与单账号不能同时下发");

  f.review.renderStatus({gateway: {version: "1.14.17", listen: "127.0.0.1:8317", modelSource: "x"},
    updatedAt: 1791980005, accounts: [], models: []});
  const meta = f.element("meta").innerHTML;
  assert(meta.includes('href="https://github.com/CangShui/workbuddy-gateway"'), "版本号必须指向项目地址");
  assert(meta.includes('target="_blank"'), "必须在新标签页打开");
  assert(meta.includes('rel="noreferrer"'), "必须带 rel=noreferrer");
  audit("告警行重排与站点筛选通过", {jump_removed: true, site_scope: true, chip_link: true});
});

test("脱敏页：标签页显示设置、汇总与记录，详情按需显示原值，退出后清空", async () => {
  const calls = [];
  const settings = {secrets: true, personal: true, words: ["SyntheticWordA"], active: true, stats: {requests: 1, values: 3}};
  const records = {days: 7, more: false, records: [
    {id: "1001", at: 1791980005000, endpoint: "/v1/messages", model: "glm-5", account: "workbuddy-a.json", values: 3,
      attempts: 2, restored: 1, failed: false, hasText: true,
      hits: [{kind: "API_KEY", placeholder: "{{API_KEY_abcdefgh}}", head: "sk-", len: 40, count: 2},
        {kind: "EMAIL", placeholder: "{{EMAIL_ijklmnop}}", head: "z", len: 26, count: 1}]}],
    summary: {requests: 1, values: 3, restored: 1, failed: 0, retried: 1, kinds: {API_KEY: 2, EMAIL: 1}, models: {"glm-5": 1},
      topValues: [{kind: "API_KEY", placeholder: "{{API_KEY_abcdefgh}}", head: "sk-", len: 40, count: 2, requests: 1}]}};
  const detail = Object.assign({}, records.records[0], {valuesInMemory: 2, traceId: "t1", requestId: 7,
    segments: [{where: "messages[0].content (user)", text: "key {{API_KEY_abcdefgh}} mail {{EMAIL_ijklmnop}}"}]});
  const f = fixture(async (url, options) => {
    calls.push({url, options});
    if (url === "/admin/api/redact") return response(200, settings);
    if (url.indexOf("/admin/api/redact-records?") === 0) return response(200, records);
    if (url.indexOf("/admin/api/redact-record?") === 0) return response(200, detail);
    if (url === "/admin/api/redact-reveal") return response(200, {values: {"{{API_KEY_abcdefgh}}": "sk-<b>synthetic</b>"}});
    if (url === "/admin/api/redact-records-clear") return response(200, {cleared: true});
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin";
  f.element("redactDays").value = "7";
  f.review.setTab("redact");
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));
  assert.equal(f.review.state.activeTab, "redact");
  assert.equal(f.element("redactState").textContent, "已开启");
  assert.equal(f.element("redactWords").value, "SyntheticWordA");
  assert(calls.some(c => c.url === "/admin/api/redact-records?days=7&limit=50"), "进入标签页必须读取记录");
  assert(f.element("redactSummary").innerHTML.includes("脱敏请求"), "必须显示汇总卡片");
  const rows = f.element("redactRecordsTable").innerHTML;
  assert(rows.includes("glm-5") && rows.includes("API Key ×2") && rows.includes("已脱敏发送"), "记录行必须显示模型、类型与状态");
  assert(!rows.includes("sk-<b>"), "列表不得出现原值");
  assert(f.element("redactKindFilter").innerHTML.includes('value="API_KEY"'), "类型筛选来自汇总");
  assert(f.element("redactTopTable").innerHTML.includes("sk-…（40 字符）"), "最常脱敏的值只显示提示");

  await f.review.openRedactDetail("1001");
  assert(!f.element("redactDetailCard").classList.contains("hidden"));
  assert(f.element("redactSegments").innerHTML.includes("<mark>{{API_KEY_abcdefgh}}</mark>"), "上游内容高亮占位符");
  assert(f.element("redactHitsTable").innerHTML.includes("已隐藏"), "默认不显示原值");
  assert(!calls.some(c => c.url === "/admin/api/redact-reveal"), "未点击前不得请求原值");

  await f.review.toggleRedactReveal();
  const reveal = calls.find(c => c.url === "/admin/api/redact-reveal");
  assert.deepEqual(JSON.parse(reveal.options.body), {id: "1001"});
  const shown = f.element("redactSegments").innerHTML;
  assert(shown.includes('<mark class="value" title="{{API_KEY_abcdefgh}}">sk-&lt;b&gt;synthetic&lt;/b&gt;</mark>'), "原值必须转义后显示");
  assert(shown.includes("<mark>{{EMAIL_ijklmnop}}</mark>"), "内存中没有的原值保持占位符");
  await f.review.toggleRedactReveal();
  assert(!f.element("redactSegments").innerHTML.includes("synthetic"), "再次点击隐藏原值");

  f.element("redactSecrets").checked = false;
  f.element("redactPersonal").checked = true;
  f.element("redactWords").value = " SyntheticWordC \n\nSyntheticWordD";
  await f.review.saveRedact();
  const save = calls.find(c => c.url === "/admin/api/redact" && c.options.method === "POST");
  assert.deepEqual(JSON.parse(save.options.body), {secrets: false, personal: true, words: ["SyntheticWordC", "SyntheticWordD"]});
  const posts = calls.filter(c => c.url === "/admin/api/redact" && c.options.method === "POST").length;
  f.element("redactWords").value = "x";
  await f.review.saveRedact();
  assert.equal(calls.filter(c => c.url === "/admin/api/redact" && c.options.method === "POST").length, posts, "过短词条不提交");

  await f.review.clearRedactRecords();
  assert(calls.some(c => c.url === "/admin/api/redact-records-clear" && c.options.body === "{}"), "清空记录必须提交空对象");
  assert(f.element("redactDetailCard").classList.contains("hidden"), "清空后关闭详情");

  await f.review.openRedactDetail("1001");
  await f.review.toggleRedactReveal();
  f.review.lockDashboard();
  assert.equal(f.review.state.redactRevealed, null, "退出后丢弃已显示的原值");
  assert(f.element("redactDetailCard").classList.contains("hidden"));
  assert.equal(f.element("redactSegments").innerHTML, "");
  assert.equal(f.element("redactWords").value, "");
  audit("脱敏页通过", {tab: true, records: true, reveal_on_demand: true, lock_reset: true});
});