(function () {
  "use strict";

  var KEY_STORAGE = "wb_gateway_admin_key";
  var POLL_MS = 3000;
  var LIST_FILTERS = [
    {kind: "accounts", id: "onlyUsableAccounts", cookie: "gateway_accounts_only_usable",
      route: "/admin/api/status", statusID: "accountFilterStatus"},
    {kind: "models", id: "onlyUsable", cookie: "gateway_models_only_usable",
      route: "/admin/api/models", statusID: "modelFilterStatus"}
  ];

  var state = {
    apiKey: sessionStorage.getItem(KEY_STORAGE) || "",
    activeTab: "overview",
    lastStatus: null,
    lastModels: null,
    timer: null,
    refreshPending: false,
    settings: null,
    togglePending: false,
    policyVersion: 0,
    logSettings: null,
    logSettingsVersion: 0,
    settingsLoadVersion: 0,
    tasksLastPoll: 0,
    configMode: "preview",
    configPending: false,
    configVersion: 0,
    configPreviewVersion: 0,
    configRevision: "",
    configOriginal: "",
    rateDetails: null,
    rateDetailsAt: 0,
    rateDetailsPending: null,
    openRateKeys: {},
    openRawLogs: {}
  };

  function $(id) { return document.getElementById(id); }

  function esc(v) {
    return String(v === undefined || v === null ? "" : v)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  function num(v) {
    return (v === undefined || v === null || v === "") ? "-" : v;
  }

  function count(v) { return v === undefined || v === null ? 0 : v; }

  function fmtTime(unix) {
    if (!unix) return "-";
    var d = new Date(unix * 1000);
    if (isNaN(d.getTime())) return "-";
    return d.toLocaleString("zh-CN", { hour12: false });
  }

  function fmtMs(v, has) {
    if (!has || v === undefined || v === null) return "-";
    return v < 1000 ? v + "ms" : (v / 1000).toFixed(1) + "s";
  }

  function fmtTokensM(v) {
    if (!v) return "0.00M";
    return (v / 1000000).toFixed(2) + "M";
  }

  function showAlert(msg, ok) {
    var el = $("alert");
    el.textContent = msg;
    el.className = "alert" + (ok ? " ok" : "");
  }

  function hideAlert() { $("alert").className = "alert hidden"; }

  // setConn 更新右上角常驻连接状态，让每次请求的结果都可见（不会一闪而过）。
  function setConn(text, cls) {
    var el = $("conn");
    if (!el) return;
    el.textContent = text;
    el.className = "conn" + (cls ? " " + cls : "");
  }

  function nowText() {
    return new Date().toLocaleTimeString("zh-CN", { hour12: false });
  }

  function traceID() {
    return "web-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2);
  }

  function reportUIEvent(event, path, status, trace, enabled) {
    if (!state.apiKey || path === "/admin/api/setup") return;
    var record = {event: event, route: path.split("?")[0], status: status};
    if (typeof enabled === "boolean") record.enabled = enabled;
    fetch("/admin/api/client-events", {
      method: "POST",
      headers: {"Authorization": "Bearer " + state.apiKey, "Content-Type": "application/json",
        "X-Workbuddy-Admin": "1", "X-Trace-ID": trace},
      body: JSON.stringify(record)
    }).catch(function () { /* 原请求失败仍由服务端审计记录，不递归发送失败事件。 */ });
  }

  function lockDashboard() {
    resetConfigEditor();
    state.tasksLastPoll = 0;
    $("tabs").classList.add("hidden");
    $("dashboard").classList.add("hidden");
    state.lastStatus = null;
    state.lastModels = null;
    state.rateDetails = null;
    state.rateDetailsAt = 0;
    state.rateDetailsPending = null;
    state.openRateKeys = {};
    state.openRawLogs = {};
    ["meta", "summaryCards", "siteSummary", "gatewayInfo", "logBox", "configBox", "tasksInfo"].forEach(function (id) {
      $(id).textContent = "";
    });
    ["accountsTable", "modelsTable", "tasksTable"].forEach(function (id) {
      $(id).querySelector("tbody").innerHTML = "";
    });
  }

  function api(path, options) {
    options = options || {};
    var trace = options.trace || traceID();
    var keyAtStart = state.apiKey;
    var headers = {"X-Trace-ID": trace};
    if (state.apiKey) headers["Authorization"] = "Bearer " + state.apiKey;
    if (options.method === "POST") {
      headers["Content-Type"] = "application/json";
      headers["X-Workbuddy-Admin"] = "1";
    }
    var status = 0;
    return fetch(path, {headers: headers, method: options.method || "GET", cache: "no-store",
      body: options.body === undefined ? undefined : JSON.stringify(options.body)}).then(function (resp) {
      status = resp.status;
      return resp.json().then(function (data) { return {resp: resp, data: data}; });
    }).then(function (result) {
      if (keyAtStart !== state.apiKey) throw new Error("登录状态已变化，忽略旧请求结果");
      var resp = result.resp;
      if (resp.status === 401) {
        state.apiKey = "";
        sessionStorage.removeItem(KEY_STORAGE);
        lockDashboard();
        setConn("鉴权失败 (401)", "danger");
        showAlert("管理鉴权失败：请输入 config.json 中的 gateway.adminKey，模型 API Key 不能用于管理登录。");
        throw new Error("管理鉴权失败");
      }
      if (!resp.ok) {
        setConn("请求失败 (" + resp.status + ")", "danger");
        showAlert("HTTP " + resp.status + "：" + (result.data.error || "请求失败") +
          (path.split("?")[0] === "/admin/api/config-edit" ? "；操作未完成，已有编辑输入保留。" : "；已有数据保留，未更新为零。"));
        var failure = new Error("HTTP " + resp.status);
        failure.status = resp.status;
        failure.detail = result.data.error || "请求失败";
        throw failure;
      }
      hideAlert();
      setConn("已连接 · " + nowText(), "ok");
      reportUIEvent("request_completed", path, status, trace);
      return result.data;
    }).catch(function (err) {
      if (keyAtStart === state.apiKey) {
        reportUIEvent("request_failed", path, status, trace);
        if (!status) {
          setConn("连接失败", "danger");
          showAlert("请求未完成；保留上次数据，请检查网络后重试。");
        }
      }
      throw err;
    });
  }

  // ---- 渲染：总览 --------------------------------------------------------
  var SUMMARY_FIELDS = [
    ["账号总数", "total", ""],
    ["可用", "active", "ok"],
    ["冷却", "cooldown", "warn"],
    ["付费耗尽", "paidExhausted", "warn"],
    ["已过期", "expired", "warn"],
    ["失效", "disabled", "danger"],
    ["已停用", "paused", "warn"]
  ];

  function metricCards(s) {
    return SUMMARY_FIELDS.map(function (c) {
      var v = s[c[1]];
      return '<div class="metric ' + c[2] + '"><div class="metric-label">' + esc(c[0]) +
        '</div><div class="metric-value">' + esc(v === undefined || v === null ? 0 : v) + '</div></div>';
    }).join("");
  }

  var SITE_LABEL = { cn: "国内站", intl: "国际站" };

  // renderSiteSummary 按站点分别展示同样的数量指标，便于一眼看出国内/国际各自的负荷。
  function renderSiteSummary(bySite) {
    var box = $("siteSummary");
    if (!box) return;
    bySite = bySite || {};
    box.innerHTML = ["cn", "intl"].map(function (site) {
      return '<div class="site-block">' +
        '<div class="site-head">' + esc(SITE_LABEL[site]) + '</div>' +
        '<div class="cards">' + metricCards(bySite[site] || {}) + '</div>' +
        '</div>';
    }).join("");
  }

  function renderStatus(data) {
    state.lastStatus = data;
    var g = data.gateway || {};
    $("meta").innerHTML =
      '<span class="chip">v' + esc(g.version) + '</span>' +
      '<span class="chip">' + esc(g.listen) + '</span>' +
      '<span class="chip">模型源 ' + esc(g.modelSource || "unavailable") + '</span>' +
      '<span class="chip">更新 ' + esc(fmtTime(data.updatedAt)) + '</span>';

    $("summaryCards").innerHTML = metricCards(data.summary || {});
    renderSiteSummary(data.siteSummary);

    $("gatewayInfo").innerHTML = kvRow("版本", g.version) +
      kvRow("监听地址", g.listen) +
      kvRow("API 鉴权", g.apiKeyEnabled ? "已启用" : "未启用") +
      kvRow("管理台", g.webuiEnabled ? "已启用" : "未启用") +
      kvRow("模型来源", g.modelSource || "unavailable") +
      kvRow("快照更新", fmtTime(data.updatedAt));

    renderAccounts(data.accounts || []);
    if (data.stale) showAlert("状态快照超过 15 秒未更新，当前展示的是上次数据，请检查网关状态。");
  }

  function kvRow(k, v) {
    return '<div class="kv-row"><span class="kv-k">' + esc(k) + '</span><span class="kv-v">' + esc(v) + '</span></div>';
  }

  var STATE_TEXT = {
    active: "可用", cooldown: "冷却", paid_exhausted: "付费耗尽",
    quota_exhausted: "付费耗尽", expired: "已过期", disabled: "失效"
  };
  var STATE_CLASS = {
    active: "ok", cooldown: "warn", paid_exhausted: "warn",
    quota_exhausted: "warn", expired: "warn", disabled: "danger"
  };

  // Cookie 仅保存两个显示偏好，不包含管理 Key、凭据、配置或账号信息。
  function readListFilterCookie(name) {
    try {
      var cookies = String(document.cookie || "").split(";");
      for (var i = 0; i < cookies.length; i++) {
        var pair = cookies[i].trim();
        if (pair.indexOf(name + "=") !== 0) continue;
        var value = pair.slice(name.length + 1);
        return value === "1" ? true : value === "0" ? false : null;
      }
    } catch (_) { /* Cookie 被禁用时回落到显示全部，不影响管理登录。 */ }
    return null;
  }

  function writeListFilterCookie(name, enabled) {
    try {
      document.cookie = name + "=" + (enabled ? "1" : "0") +
        "; Path=/ui; Max-Age=31536000; SameSite=Lax" +
        (window.location.protocol === "https:" ? "; Secure" : "");
      return readListFilterCookie(name) === enabled;
    } catch (_) { return false; }
  }

  function initListFilters() {
    LIST_FILTERS.forEach(function (filter) {
      $(filter.id).checked = readListFilterCookie(filter.cookie) === true;
      $(filter.id).addEventListener("change", function () { changeListFilter(filter.kind); });
    });
  }

  function reportRestoredListFilters() {
    LIST_FILTERS.forEach(function (filter) {
      reportUIEvent("list_filter_restored", filter.route, 0, traceID(), $(filter.id).checked);
    });
  }

  function changeListFilter(kind) {
    var filter = LIST_FILTERS.find(function (item) { return item.kind === kind; });
    if (!filter) return;
    var trace = traceID();
    var enabled = $(filter.id).checked;
    var remembered = writeListFilterCookie(filter.cookie, enabled);
    var hint = $(filter.statusID);
    hint.textContent = remembered ? "" : "本次筛选已应用，但浏览器未允许保存 Cookie，刷新后可能无法记住状态。";
    hint.classList.toggle("hidden", remembered);
    reportUIEvent("list_filter_changed", filter.route, 0, trace, enabled);
    if (!remembered) reportUIEvent("list_filter_cookie_failed", filter.route, 0, trace, enabled);
    // 筛选立即作用于已加载的数据；刷新中的响应也按当前勾选状态渲染。
    if (kind === "accounts") {
      if (state.lastStatus) renderAccounts(state.lastStatus.accounts || []);
    } else if (state.lastModels) {
      renderModels(state.lastModels);
    }
  }

  function renderAccounts(list) {
    var onlyUsable = $("onlyUsableAccounts").checked;
    if (onlyUsable) {
      list = list.filter(function (account) {
        return account.state === "active" && account.schedulingEnabled !== false;
      });
    }
    var tb = $("accountsTable").querySelector("tbody");
    var focusTarget = focusedPolicyTarget(tb);
    if (!list.length) {
      tb.innerHTML = '<tr><td colspan="16" class="empty">' +
        (onlyUsable ? "没有符合条件的可用账号，取消勾选可查看全部账号。" : "暂无账号数据（服务是否已写入状态快照？）") + '</td></tr>';
      return;
    }
    tb.innerHTML = list.map(function (a, i) {
      var st = STATE_TEXT[a.state] || a.state || "-";
      var cls = STATE_CLASS[a.state] || "";
      if (a.schedulingEnabled === false) {
        st = a.state === "active" ? "已停用" : "已停用 · " + st;
        cls = "warn";
      }
      var site = a.edition === "intl" ? "国际站" : "国内站";
      return "<tr>" +
        "<td>" + (i + 1) + "</td>" +
        "<td>" + policySwitch("account", a.path, a.schedulingEnabled !== false, a.schedulingReason) + "</td>" +
        "<td class='mono'>" + esc(a.path) + "</td>" +
        "<td>" + esc(a.nickname || "-") + "</td>" +
        "<td>" + esc(site) + "</td>" +
        "<td><span class='badge " + cls + "'>" + esc(st) + "</span></td>" +
        "<td>" + esc(fmtTime(a.tokenExpiresAt)) + "</td>" +
        "<td>" + esc(quotaAmount(a, "total")) + "</td>" +
        "<td>" + esc(quotaAmount(a, "used")) + "</td>" +
        "<td>" + esc(quotaAmount(a, "remaining")) + "</td>" +
        "<td>" + esc((a.planLabel || "-") + (a.planStale ? "（待刷新）" : "")) + "</td>" +
        "<td>" + esc(count(a.freeModels)) + "</td>" +
        "<td>" + esc(count(a.modelCooldowns)) + "</td>" +
        "<td>" + esc(fmtTime(a.refreshExpiresAt)) + "</td>" +
        "<td>" + esc(fmtTime(a.lastRefreshTime)) + "</td>" +
        "<td>" + esc(count(a.refreshFailCount)) + "</td>" +
        "</tr>";
    }).join("");
    restorePolicyFocus(tb, focusTarget);
  }

  function quotaAmount(account, kind) {
    if (!account.quotaKnown) return "-";
    if (account.quotaUnlimited && (kind === "total" || kind === "remaining")) return "不限";
    var value = kind === "total" ? account.quotaTotal : (kind === "used" ? account.quotaUsed : account.quotaRemaining);
    return count(value);
  }

  // ---- 渲染：模型 --------------------------------------------------------
  function focusedPolicyTarget(table) {
    var element = document.activeElement;
    return element && table.contains(element) && element.classList.contains("policy-switch") ?
      element.getAttribute("data-target") : null;
  }

  function restorePolicyFocus(table, target) {
    if (target === null) return;
    table.querySelectorAll(".policy-switch").forEach(function (button) {
      if (button.getAttribute("data-target") === target) button.focus({preventScroll: true});
    });
  }

  function policySwitch(kind, target, enabled, reason) {
    return '<button type="button" role="switch" class="policy-switch" data-kind="' + esc(kind) +
      '" data-target="' + esc(target) + '" aria-checked="' + enabled + '" aria-label="' +
      esc((kind === "account" ? "账号 " : "模型 ") + target + " 调度开关") +
      '" title="' + esc(reason || (enabled ? "允许调度" : "已停用")) + '"' +
      (state.togglePending ? " disabled" : "") + '><span class="switch-track" aria-hidden="true"></span>' +
      (enabled ? "已开启" : "已停用") + '</button>';
  }

  function togglePolicy(event) {
    var button = event.target.closest("button.policy-switch");
    if (!button || state.togglePending || !state.apiKey) return;
    var kind = button.getAttribute("data-kind");
    var target = button.getAttribute("data-target");
    var enabled = button.getAttribute("aria-checked") !== "true";
    var endpoint = "/admin/api/" + kind + "-toggle";
    state.togglePending = true;
    state.policyVersion++;
    document.querySelectorAll(".policy-switch").forEach(function (b) { b.disabled = true; });
    return api(endpoint, {method: "POST", body: {target: target, enabled: enabled}})
      .then(function () {
        // 保存成功才修改控件，不将失败伪装成已生效。
        button.setAttribute("aria-checked", String(enabled));
        button.lastChild.textContent = enabled ? "已开启" : "已停用";
        showAlert("已写入 config.json，" + (kind === "account" ? "账号全局调度" : "模型") +
          (enabled ? "已开启。" : "已停用。") + "已有请求不被中断。", true);
      }).catch(function () {}).then(function () {
        state.togglePending = false;
        state.policyVersion++;
        document.querySelectorAll(".policy-switch").forEach(function (b) { b.disabled = false; });
        refreshActive();
      });
  }

  function renderModels(data) {
    state.lastModels = data;
    var list = data.models || [];
    var onlyUsable = $("onlyUsable").checked;
    if (onlyUsable) {
      list = list.filter(function (m) { return m.enabled !== false && (m.availableAccounts || 0) > 0; });
    }
    var tb = $("modelsTable").querySelector("tbody");
    var focusTarget = focusedPolicyTarget(tb);
    $("modelPolicyHint").textContent = data.policyMode === "allowlist" ?
      "当前使用模型白名单，开关反映白名单规则。首次修改开关会删除 models.allowlist，将当前模型列表的启停状态转为 models.blocklist；以后新增模型默认开启。" :
      "模型开关写入 models.blocklist 并立即生效；停用模型保留统计记录，取消筛选可查看全部模型，客户端模型目录会隐藏停用项。";
    if (!list.length) {
      tb.innerHTML = '<tr><td colspan="10" class="empty">' +
        (onlyUsable ? "没有符合条件的可用模型，取消勾选可查看全部模型。" : "暂无模型数据") + '</td></tr>';
      return;
    }
    tb.innerHTML = list.map(function (m) {
      return "<tr>" +
        "<td>" + policySwitch("model", m.id, m.enabled !== false, m.policyReason) + "</td>" +
        "<td class='mono'>" + esc(m.id) + "</td>" +
        rateCell("cn", m.id, m.cnMultiplier) +
        rateCell("intl", m.id, m.intlMultiplier) +
        "<td>" + esc(count(m.availableAccounts)) + "</td>" +
        "<td>" + esc(count(m.requests)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgTtftMs, m.hasTtft)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgLatencyMs, m.hasLatency)) + "</td>" +
        "<td>" + esc(fmtTokensM(m.tokens)) + "</td>" +
        "<td>" + esc(m.lastStatus || "-") + "</td>" +
        "</tr>";
    }).join("");
    restorePolicyFocus(tb, focusTarget);
    restoreRateDetails(tb);
    if (data.stale) showAlert("模型统计快照过旧，当前展示的是上次数据。");
  }

  // ---- 倍率来源：全部取自普通日志，没有记录就显示「没有日志」 -------------------
  function rateKey(site, model) {
    return site + "|" + String(model || "").trim().toLowerCase();
  }

  function rateCell(site, model, text) {
    var label = site === "cn" ? "国内" : "国际";
    return "<td class='rate-cell'><div class='rate-line'><span class='rate-value'>" + esc(text || "-") +
      "</span><button type='button' class='rate-info' data-rate-site='" + esc(site) +
      "' data-rate-model='" + esc(model) + "' aria-expanded='false' aria-label='查看" + label +
      "倍率数据来源与更新时间'>!</button></div>" +
      "<div class='rate-detail hidden' role='region' aria-label='" + label + "倍率来源详情'></div></td>";
  }

  function ensureRateDetails() {
    if (state.rateDetails && Date.now() - state.rateDetailsAt < 60000) return Promise.resolve(state.rateDetails);
    if (state.rateDetailsPending) return state.rateDetailsPending;
    state.rateDetailsPending = api("/admin/api/rate-detail").then(function (data) {
      state.rateDetails = data;
      state.rateDetailsAt = Date.now();
      state.rateDetailsPending = null;
      return data;
    }).catch(function (err) {
      state.rateDetailsPending = null;
      throw err;
    });
    return state.rateDetailsPending;
  }

  function rateDetailHTML(payload, site, model) {
    var info = payload && payload.rates ? payload.rates[rateKey(site, model)] : null;
    if (!info || !info.hasLog) {
      return "<p class='rate-none'>没有日志</p><p class='hint'>" +
        esc(payload && payload.noLogHint || "普通日志中没有该模型的倍率记录。") + "</p>";
    }
    // 不再标注「状态：未知」：该结论是实测过的，写「未知」会让人误以为从未检测。
    var meta = "<div class='rate-meta'><span>来源：" + esc(info.source) + "</span>" +
      "<span>更新时间：" + esc(fmtTime(info.updatedAt)) + "</span></div>";
    var html = meta;
    if (info.explain) html += "<p class='rate-explain'>" + esc(info.explain) + "</p>";
    if (info.unknown && info.refreshHint) {
      html += "<p class='rate-refresh'><strong>如何刷新：</strong>" + esc(info.refreshHint) + "</p>";
    }
    var others = info.others || [];
    if (others.length) {
      html += "<div class='rate-others'><div class='rate-logs-title'>其他记录</div><ul>" +
        others.map(function (record) {
          return "<li>" + esc(record.source) + " · " + esc(fmtTime(record.updatedAt)) + " · " +
            esc(record.explain || record.value || "") + "</li>";
        }).join("") + "</ul></div>";
    }
    var raw = (info.records || []).filter(function (record) { return record.logLine; });
    if (raw.length) {
      // 轮询会重建表格，展开状态必须自己记住，否则每次刷新都会被自动折叠。
      var rawOpen = state.openRawLogs[rateKey(site, model)] === true;
      html += "<details class='rate-raw'" + (rawOpen ? " open" : "") +
        "><summary>原始日志（" + raw.length + " 条，默认折叠）</summary><ul>" +
        raw.map(function (record) { return "<li><code>" + esc(record.logLine) + "</code></li>"; }).join("") +
        "</ul></details>";
    }
    return html;
  }

  function fillRateDetail(button, panel) {
    var site = button.getAttribute("data-rate-site");
    var model = button.getAttribute("data-rate-model");
    panel.classList.remove("hidden");
    button.setAttribute("aria-expanded", "true");
    panel.innerHTML = "<p class='hint'>读取日志中…</p>";
    ensureRateDetails().then(function (payload) {
      panel.innerHTML = rateDetailHTML(payload, site, model);
      // details 的 toggle 事件不冒泡，只能在渲染后逐个绑定，用于记住展开状态。
      var details = panel.querySelector("details.rate-raw");
      if (details) {
        details.addEventListener("toggle", function () {
          var key = rateKey(site, model);
          if (details.open) {
            state.openRawLogs[key] = true;
          } else {
            delete state.openRawLogs[key];
          }
        });
      }
    }).catch(function () {
      panel.innerHTML = "<p class='hint'>日志读取失败，请稍后重试；没有数据时不猜测来源。</p>";
    });
  }

  function toggleRateDetail(event) {
    var button = event.target.closest("button.rate-info");
    if (!button) return;
    var cell = button.closest("td");
    var panel = cell ? cell.querySelector(".rate-detail") : null;
    if (!panel) return;
    var key = rateKey(button.getAttribute("data-rate-site"), button.getAttribute("data-rate-model"));
    if (button.getAttribute("aria-expanded") === "true") {
      button.setAttribute("aria-expanded", "false");
      panel.classList.add("hidden");
      panel.innerHTML = "";
      delete state.openRateKeys[key];
      return;
    }
    state.openRateKeys[key] = true;
    fillRateDetail(button, panel);
  }

  // 轮询会重建表格，已展开的详情按 key 恢复，避免每次刷新都折叠。
  function restoreRateDetails(table) {
    Array.prototype.forEach.call(table.querySelectorAll("button.rate-info"), function (button) {
      var key = rateKey(button.getAttribute("data-rate-site"), button.getAttribute("data-rate-model"));
      if (!state.openRateKeys[key]) return;
      var cell = button.closest("td");
      var panel = cell ? cell.querySelector(".rate-detail") : null;
      if (panel) fillRateDetail(button, panel);
    });
  }

  // ---- 渲染：日志 --------------------------------------------------------
  function loadLogs(initial) {
    var file = initial ? "" : $("logFile").value;
    var lines = $("logLines").value;
    var q = "/admin/api/logs?lines=" + encodeURIComponent(lines);
    if (file) q += "&file=" + encodeURIComponent(file);
    return api(q).then(function (data) {
      var sel = $("logFile");
      if (initial && data.files) {
        sel.innerHTML = data.files.map(function (f) {
          return '<option value="' + esc(f.name) + '">' + esc(f.name) + '</option>';
        }).join("");
        if (data.file) sel.value = data.file;
      }
      var box = $("logBox");
      if (data.error) { box.textContent = "读取日志失败: " + data.error; return; }
      box.textContent = (data.lines && data.lines.length) ? data.lines.join("\n") : "（暂无日志）";
      if (data.truncated) showAlert("日志达到管理界面的读取预算，只展示完整尾部行；超长单行被省略。原日志文件未修改。");
    }).catch(function () {
      // api 已显示错误，保留上次日志，不把读取失败伪装成空数据。
      if (!initial && file) return loadLogs(true); // 当前文件被自动清理时重新选择现有日志。
    });
  }

  // ---- 渲染：配置 --------------------------------------------------------
  function loadConfig() {
    var version = ++state.configPreviewVersion;
    return api("/admin/api/config").then(function (data) {
      if (version !== state.configPreviewVersion) return;
      var box = $("configBox");
      if (!data.exists) { box.textContent = "未找到 " + (data.path || "config.json"); return; }
      box.textContent = data.content;
    }).catch(function () {
      // 保留上次内容。
    });
  }

  function configDirty() {
    return state.configMode === "edit" && $("configEditor").value !== state.configOriginal;
  }

  function setConfigBusy(busy) {
    state.configPending = busy;
    ["editConfig", "acknowledgeConfigEdit", "saveConfig", "reloadConfigEditor"].forEach(function (id) {
      $(id).disabled = busy;
    });
    $("configEditor").readOnly = busy;
    $("saveConfig").textContent = busy && state.configMode === "edit" ? "处理中…" : "校验并保存";
  }

  function resetConfigEditor() {
    state.configVersion++;
    state.configMode = "preview";
    state.configRevision = "";
    state.configOriginal = "";
    $("configEditor").value = "";
    $("configEditorStatus").textContent = "";
    $("configWarning").classList.add("hidden");
    $("configEditorPanel").classList.add("hidden");
    $("configBox").classList.remove("hidden");
    $("editConfig").classList.remove("hidden");
    $("configTitle").textContent = "config.json（预览 · Key 已隐藏）";
    $("cancelConfigEdit").disabled = false;
    setConfigBusy(false);
  }

  function showConfigWarning() {
    if (state.configPending || state.configMode !== "preview") return;
    hideAlert();
    state.configMode = "warning";
    $("configResult").textContent = "";
    $("editConfig").classList.add("hidden");
    $("configWarning").classList.remove("hidden");
    reportUIEvent("config_edit_warning", "/admin/api/config-edit", 0, traceID());
    $("acknowledgeConfigEdit").focus();
  }

  function exitConfigEdit() {
    if (state.configPending && state.configMode === "edit") return false;
    if (configDirty() && !window.confirm("有未保存的配置修改。退出会丢弃这些修改，是否继续？")) return false;
    reportUIEvent("config_edit_cancelled", "/admin/api/config-edit", 0, traceID());
    resetConfigEditor();
    hideAlert();
    return true;
  }

  function loadConfigEditor() {
    if (state.configPending || state.configMode === "preview") return Promise.resolve();
    if (configDirty() && !window.confirm("重新加载会丢弃未保存的修改，是否继续？")) return Promise.resolve();
    var version = ++state.configVersion;
    var trace = traceID();
    setConfigBusy(true);
    reportUIEvent("config_edit_acknowledged", "/admin/api/config-edit", 0, trace);
    $("configEditorStatus").textContent = "正在加载服务器配置…";
    $("configResult").textContent = "正在加载服务器配置…";
    return api("/admin/api/config-edit?acknowledged=1", {trace: trace}).then(function (data) {
      if (version !== state.configVersion) return;
      state.configRevision = data.revision;
      state.configOriginal = data.content;
      state.configMode = "edit";
      $("configEditor").value = data.content;
      $("configWarning").classList.add("hidden");
      $("configBox").classList.add("hidden");
      $("configEditorPanel").classList.remove("hidden");
      $("editConfig").classList.add("hidden");
      $("configTitle").textContent = "config.json（编辑 · 包含真实 Key）";
      $("configEditorStatus").textContent = "已加载原文。保存前会校验配置和版本；其他页面或服务器修改过配置时将拒绝覆盖。";
      $("configResult").textContent = "";
    }).catch(function (err) {
      if (version !== state.configVersion) return;
      var message = "加载失败：" + (err.detail || err.message) + "。已有输入保留。";
      $("configEditorStatus").textContent = message;
      $("configResult").textContent = message;
    }).then(function () {
      if (version !== state.configVersion) return;
      setConfigBusy(false);
      if (state.configMode === "edit") $("configEditor").focus();
    });
  }

  function saveConfigEditor() {
    if (state.configPending || state.configMode !== "edit") return Promise.resolve();
    var content = $("configEditor").value;
    var trace = traceID();
    var parsed;
    try {
      parsed = JSON.parse(content);
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("必须是单个 JSON 对象");
      if (new TextEncoder().encode(content).length > 1048576) throw new Error("超过 1 MiB");
    } catch (_) {
      $("configEditorStatus").textContent = "配置未保存：请输入不超过 1 MiB 的完整 JSON 对象，请检查引号、逗号和括号。";
      reportUIEvent("config_validation_failed", "/admin/api/config-edit", 400, trace);
      return Promise.resolve();
    }
    var nextAdminKey = parsed.gateway && parsed.gateway.adminKey || "";
    var adminKeyChanged = nextAdminKey !== state.apiKey;
    var removal = !nextAdminKey;
    if (removal && !window.confirm("你正在清空管理 Key（或删除 gateway.adminKey）。保存后管理登录将不可用，需要在服务器本机重新设置。确认清空？")) {
      reportUIEvent("config_edit_cancelled", "/admin/api/config-edit", 0, trace);
      return Promise.resolve();
    }
    if (!window.confirm("确认将整份配置写入服务器 config.json？\n密钥、规则和提示词等立即生效，端口修改需重启。日志保留规则可能触发不可撤销的删除。" +
        (adminKeyChanged ? "\n管理 Key 已修改，保存后需要重新登录，请先妥善保存新 Key。" : ""))) {
      reportUIEvent("config_edit_cancelled", "/admin/api/config-edit", 0, trace);
      return Promise.resolve();
    }
    var version = state.configVersion;
    setConfigBusy(true);
    $("cancelConfigEdit").disabled = true;
    $("configEditorStatus").textContent = "正在校验并保存…";
    return api("/admin/api/config-edit", {method: "POST", trace: trace,
      body: {content: content, revision: state.configRevision, acknowledged: true, confirmAdminKeyRemoval: removal}
    }).then(function (data) {
      if (version !== state.configVersion) return;
      var message = "配置已保存，热配置已生效。" +
        (data.settings && data.settings.restartRequired ? " 监听端口变更需重启服务。" : " 当前监听端口未改变。") +
        (data.settings && data.settings.apiPortOverridden ? " API 端口受 -port 启动参数覆盖。" : "");
      resetConfigEditor();
      $("configResult").textContent = message;
      if (data.adminKeyChanged) {
        state.apiKey = "";
        sessionStorage.removeItem(KEY_STORAGE);
        $("apiKey").value = "";
        lockDashboard();
        showAlert(message + (data.adminKeyCleared ? " 管理 Key 已清空，请在服务器本机重新设置。" : " 管理 Key 已修改，请使用新 Key 重新登录。"), true);
        setConn("需要重新设置管理 Key 或登录", "");
      } else {
        state.settingsLoadVersion++;
        state.logSettingsVersion++;
        state.policyVersion++;
        loadConfig();
        loadSettings();
      }
    }).catch(function (err) {
      if (version !== state.configVersion) return;
      $("configEditorStatus").textContent = err.status === 409 ?
        "版本冲突：服务器配置已变化。修改未写入，当前输入保留。请先核对修改，再重新加载最新配置。" :
        "保存未完成：" + (err.detail || err.message) + "。当前输入保留，请检查后重试。";
    }).then(function () {
      if (version !== state.configVersion) return;
      $("cancelConfigEdit").disabled = false;
      setConfigBusy(false);
    });
  }

  function renderTasks(data) {
    var labels = {success: "成功", failed: "失败", partial: "部分失败", cancelled: "已取消",
      skipped: "跳过", running: "执行中", disabled: "未启用", unknown: "无执行记录"};
    var classes = {success: "ok", failed: "danger", partial: "warn", cancelled: "warn", running: "warn"};
    $("tasksInfo").textContent = "服务器日志时区：" + (data.timezone || "-") + "（表内时间按浏览器时区显示） · 刷新 " + fmtTime(data.generatedAt) +
      (data.truncated ? " · 日志扫描达到预算，缺失记录不代表未执行。" : " · 执行记录以日志为准。");
    $("tasksTable").querySelector("tbody").innerHTML = (data.tasks || []).map(function (task) {
      var next = task.nextRunAt ? fmtTime(task.nextRunAt) + (task.nextEstimated ? "（推算）" : "") :
        (!task.enabled ? "未启用" : task.oneShot && task.lastFinishedAt ? "本次启动已完成" : "等待调度日志");
      return "<tr><td data-label='任务'>" + esc(task.name) + "</td><td data-label='启用'>" + (task.enabled ? "开启" : "关闭") +
        "</td><td data-label='执行周期' class='task-description'>" + esc(task.schedule) + "</td><td data-label='上次执行记录'>" + esc(fmtTime(task.lastRunAt)) +
        "</td><td data-label='上次完成'>" + esc(fmtTime(task.lastFinishedAt)) + "</td><td data-label='执行结果'><span class='badge " +
        (classes[task.status] || "") + "'>" + esc(labels[task.status] || "无执行记录") +
        "</span></td><td data-label='下次预计执行'>" + esc(next) + "</td><td data-label='日志说明' class='task-description'>" + esc(task.detail || "当前服务尚无该任务日志，或日志已清理") +
        (task.logFile ? "<br><span class='mono'>" + esc(task.logFile) + "</span>" : "") + "</td></tr>";
    }).join("");
  }

  function loadLogSettings() {
    var version = ++state.logSettingsVersion;
    $("logRetentionDays").disabled = true;
    $("saveLogSettings").disabled = true;
    $("cleanupLogs").disabled = true;
    return api("/admin/api/log-settings").then(function (data) {
      if (version !== state.logSettingsVersion) return;
      state.logSettings = data;
      $("logRetentionDays").value = data.retentionDays;
    }).catch(function () {}).then(function () {
      if (version !== state.logSettingsVersion) return;
      $("logRetentionDays").disabled = false;
      $("saveLogSettings").disabled = false;
      $("cleanupLogs").disabled = false;
    });
  }

  function saveLogSettings() {
    var days = Number($("logRetentionDays").value);
    if ($("logRetentionDays").value.trim() === "" || !Number.isInteger(days) || days < 0 || days > 36500) {
      showAlert("保留天数必须是 0～36500 的整数，0 表示保留全部。");
      reportUIEvent("log_validation_failed", "/admin/api/log-settings", 0, traceID());
      return;
    }
    if (days > 0 && !window.confirm("保存后会自动删除超出 " + days + " 天的项目日志，删除不可撤销。是否继续？")) return;
    $("saveLogSettings").disabled = true;
    return api("/admin/api/log-settings", {method: "POST", body: {retentionDays: days}})
      .then(function (data) {
        state.logSettings = data;
        $("logCleanupResult").textContent = "已写入 config.json。保留天数：" + days + (days === 0 ? "（不自动清理）。" : " 天，自动清理已启用。");
      }).catch(function () {}).then(function () { $("saveLogSettings").disabled = false; });
  }

  function cleanupExpiredLogs() {
    var days = state.logSettings && state.logSettings.retentionDays;
    if (!days) {
      $("logCleanupResult").textContent = "当前配置保留全部，不会删除任何日志。请先保存正数保留天数。";
      reportUIEvent("log_validation_failed", "/admin/api/log-cleanup", 0, traceID());
      return;
    }
    if (!window.confirm("按已保存的 " + days + " 天保留规则删除过期项目日志，删除不可撤销。是否继续？")) return;
    $("cleanupLogs").disabled = true;
    return api("/admin/api/log-cleanup", {method: "POST", body: {}}).then(function (data) {
      $("logCleanupResult").textContent = "已删除 " + data.deleted + " 个日志文件，回收 " + data.deletedBytes +
        " 字节；跳过正在写入 " + data.skippedActive + " 个，失败 " + data.failed + " 个。";
      loadLogs(true);
    }).catch(function () {}).then(function () { $("cleanupLogs").disabled = false; });
  }

  // ---- 路由与轮询 --------------------------------------------------------
  function refreshActive() {
    if (!state.apiKey || state.refreshPending || state.togglePending) return Promise.resolve();
    if (state.activeTab === "tasks" && Date.now() - state.tasksLastPoll < 15000) return Promise.resolve();
    if (state.activeTab === "tasks") state.tasksLastPoll = Date.now();
    state.refreshPending = true;
    var policyVersion = state.policyVersion;
    var onErr = function () {
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("连接失败", "danger");
      }
    };
    var pending = Promise.resolve();
    if (state.activeTab === "overview" || state.activeTab === "accounts") {
      pending = api("/admin/api/status").then(function (data) {
        if (!state.togglePending && policyVersion === state.policyVersion) renderStatus(data);
      }).catch(onErr);
    } else if (state.activeTab === "models") {
      pending = api("/admin/api/models").then(function (data) {
        if (!state.togglePending && policyVersion === state.policyVersion) renderModels(data);
      }).catch(onErr);
    } else if (state.activeTab === "logs") {
      pending = loadLogs(false);
    } else if (state.activeTab === "tasks") {
      pending = api("/admin/api/tasks").then(renderTasks).catch(onErr);
    }
    return pending.then(function () {
      state.refreshPending = false;
      if (policyVersion !== state.policyVersion && !state.togglePending) return refreshActive();
    }, function () { state.refreshPending = false; });
  }

  function setTab(name) {
    if (name !== "config" && state.configMode !== "preview" && !exitConfigEdit()) return;
    if (name === "tasks") state.tasksLastPoll = 0;
    state.activeTab = name;
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.classList.toggle("active", t.getAttribute("data-tab") === name);
    });
    Array.prototype.forEach.call(document.querySelectorAll(".panel"), function (p) {
      p.classList.toggle("active", p.id === "panel-" + name);
    });
    if (name === "logs") { loadLogs(true); loadLogSettings(); }
    else if (name === "config") loadConfig();
    else if (name === "settings") loadSettings();
    else refreshActive();
  }

  function renderSettings(data) {
    state.settings = data;
    $("apiKeyEnabled").checked = data.apiKeyEnabled;
    $("modelAPIKey").value = "";
    $("apiKeyState").textContent = data.apiKeyConfigured ? "已配置 API Key，留空将保留原值。" : "尚未配置 API Key。";
    $("apiPortValue").textContent = data.apiPort;
    $("webPortValue").textContent = data.webPort;
    $("configuredApiPort").value = data.configuredApiPort || data.apiPort;
    $("configuredWebPort").value = data.configuredWebPort || data.webPort;
    $("portSettingsStatus").textContent = (data.restartRequired ? "端口配置已保存，待重启生效。 " :
      "配置端口与实际监听一致。 ") + (data.apiPortOverridden ? "API 端口被启动参数 -port 覆盖，需调整该参数才能按配置启动。 " : "") +
      "修改并保存后需重启网关，不会立即中断页面或请求。";
  }

  function loadSettings() {
    var version = ++state.settingsLoadVersion;
    setSettingsBusy(true);
    return api("/admin/api/settings").then(function (data) {
      if (version === state.settingsLoadVersion) renderSettings(data);
    }).catch(function () {}).then(function () {
      if (version === state.settingsLoadVersion) setSettingsBusy(false);
    });
  }

  function setSettingsBusy(busy) {
    ["apiKeyEnabled", "modelAPIKey", "configuredApiPort", "configuredWebPort", "saveAPISettings", "savePortSettings"]
      .forEach(function (id) { $(id).disabled = busy; });
  }

  function saveAPISettings() {
    var enabled = $("apiKeyEnabled").checked;
    var key = $("modelAPIKey").value;
    if (enabled && !key && !(state.settings && state.settings.apiKeyConfigured)) {
      showAlert("开启 API Key 校验前必须输入一个 Key。");
      reportUIEvent("settings_validation_failed", "/admin/api/settings", 0, traceID());
      return;
    }
    setSettingsBusy(true);
    return api("/admin/api/settings", {method: "POST", body: {apiKeyEnabled: enabled, apiKey: key}})
      .then(function (data) {
        renderSettings(data);
        showAlert("已写入 config.json，API 鉴权设置立即生效；管理鉴权未改变。", true);
      }).catch(function () {}).then(function () { setSettingsBusy(false); });
  }

  function savePortSettings() {
    var apiPort = Number($("configuredApiPort").value);
    var webPort = Number($("configuredWebPort").value);
    if (!Number.isInteger(apiPort) || !Number.isInteger(webPort) || apiPort < 1 || apiPort > 65535 ||
      webPort < 1 || webPort > 65535 || apiPort === webPort) {
      showAlert("端口必须是 1～65535 的整数，且 API 与 Web 端口不能相同。");
      reportUIEvent("settings_validation_failed", "/admin/api/settings", 0, traceID());
      return;
    }
    setSettingsBusy(true);
    return api("/admin/api/settings", {method: "POST", body: {apiPort: apiPort, webPort: webPort}})
      .then(function (data) {
        renderSettings(data);
        showAlert("端口已写入 config.json，重启网关后生效；当前监听端口未改变。", true);
      }).catch(function () {}).then(function () { setSettingsBusy(false); });
  }

  // 管理 Key 仅保存在当前标签页会话，验证成功后才保存。
  function saveKey() {
    if (state.configMode !== "preview" && !exitConfigEdit()) return Promise.resolve();
    var key = $("apiKey").value.trim();
    if (!key) { showAlert("请输入管理 Key。"); return Promise.resolve(); }
    state.apiKey = key;
    setConn("校验中…", "");
    return api("/admin/api/settings").then(function (data) {
      sessionStorage.setItem(KEY_STORAGE, key);
      reportRestoredListFilters();
      renderSettings(data);
      $("tabs").classList.remove("hidden");
      $("dashboard").classList.remove("hidden");
      $("setupPanel").classList.add("hidden");
      showAlert("管理鉴权通过。", true);
      refreshActive();
    }).catch(function () {
      // api() 已提示 401 / HTTP 错误；这里补充网络类错误。
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("连接失败", "danger");
        showAlert("无法连接管理端口，请检查访问地址与 Web 端口。");
      }
    });
  }

  function initializeAdmin() {
    $("initializeAdmin").disabled = true;
    return api("/admin/api/setup", {method: "POST", body: {}}).then(function (data) {
      // 原文仅本次弹窗显示，不写 console、日志或 localStorage。
      window.alert("管理 Key 已成功写入 config.json：\n\n" + data.adminKey +
        "\n\n请妥善保存。这是唯一一次自动展示；遗忘时请在服务器本机查看 config.json。");
      $("apiKey").value = data.adminKey;
      return saveKey();
    }).catch(function () {}).then(function () { $("initializeAdmin").disabled = false; });
  }

  function discoverSetup() {
    return api("/admin/api/setup").then(function (data) {
      if (data.setupRequired) {
        state.apiKey = "";
        sessionStorage.removeItem(KEY_STORAGE);
        lockDashboard();
        $("setupPanel").classList.remove("hidden");
        $("initializeAdmin").disabled = !data.canInitialize;
        setConn("等待初始化", "");
        if (!data.canInitialize) {
          showAlert("当前不是本机访问，无法在此页面自动生成管理 Key。可选方式：" +
            "① 在服务器本机打开本页面；② 用 SSH 本地端口转发（ssh -L 8316:127.0.0.1:8316 用户@服务器）后访问；" +
            "③ 直接在服务器上编辑 config.json，把 gateway.adminKey 设为随机字符串，保存后约 1 秒生效。");
        }
      } else if (state.apiKey) {
        saveKey();
      } else {
        lockDashboard();
        setConn("等待管理登录", "");
        showAlert("请输入 config.json 中的 gateway.adminKey。");
      }
    }).catch(function () {});
  }

  function initTheme() {
    var saved = localStorage.getItem("wb_theme");
    if (saved) document.documentElement.setAttribute("data-theme", saved);
    $("themeToggle").addEventListener("click", function () {
      var cur = document.documentElement.getAttribute("data-theme");
      var next = cur === "dark" ? "light" : (cur === "light" ? "dark" : (matchMedia("(prefers-color-scheme: dark)").matches ? "light" : "dark"));
      document.documentElement.setAttribute("data-theme", next);
      localStorage.setItem("wb_theme", next);
    });
  }

  function init() {
    // 清理旧版同源页面遗留的模型 API Key，不把它误当成管理 Key。
    localStorage.removeItem("wb_gateway_api_key");
    initListFilters();
    $("apiKey").value = state.apiKey;
    initTheme();
    $("saveKey").addEventListener("click", saveKey);
    $("logout").addEventListener("click", function () {
      if (state.configMode !== "preview" && !exitConfigEdit()) return;
      resetConfigEditor();
      sessionStorage.removeItem(KEY_STORAGE);
      state.apiKey = "";
      window.location.reload();
    });
    $("initializeAdmin").addEventListener("click", initializeAdmin);
    $("saveAPISettings").addEventListener("click", saveAPISettings);
    $("savePortSettings").addEventListener("click", savePortSettings);
    $("editConfig").addEventListener("click", showConfigWarning);
    $("acknowledgeConfigEdit").addEventListener("click", loadConfigEditor);
    $("cancelConfigWarning").addEventListener("click", exitConfigEdit);
    $("cancelConfigEdit").addEventListener("click", exitConfigEdit);
    $("reloadConfigEditor").addEventListener("click", loadConfigEditor);
    $("saveConfig").addEventListener("click", saveConfigEditor);
    $("configEditor").addEventListener("input", function () {
      $("configEditorStatus").textContent = configDirty() ? "有未保存的修改。离开配置页会提示确认；不会自动保存。" : "内容未修改。";
    });
    window.addEventListener("beforeunload", function (event) {
      if (!configDirty() && !state.configPending) return;
      event.preventDefault();
      event.returnValue = "";
    });
    $("saveLogSettings").addEventListener("click", saveLogSettings);
    $("cleanupLogs").addEventListener("click", cleanupExpiredLogs);
    $("apiKey").addEventListener("keydown", function (e) { if (e.key === "Enter") saveKey(); });
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.addEventListener("click", function () { setTab(t.getAttribute("data-tab")); });
    });
    $("accountsTable").addEventListener("click", togglePolicy);
    $("modelsTable").addEventListener("click", togglePolicy);
    $("modelsTable").addEventListener("click", toggleRateDetail);
    $("logFile").addEventListener("change", function () { loadLogs(false); });
    $("logLines").addEventListener("change", function () { loadLogs(false); });

    discoverSetup();
    state.timer = setInterval(refreshActive, POLL_MS);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
