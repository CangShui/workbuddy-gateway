(function () {
  "use strict";

  var KEY_STORAGE = "wb_gateway_admin_key";
  var POLL_MS = 3000;

  var state = {
    apiKey: sessionStorage.getItem(KEY_STORAGE) || "",
    activeTab: "overview",
    lastStatus: null,
    timer: null,
    refreshPending: false,
    settings: null
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

  function reportUIEvent(event, path, status, trace) {
    if (!state.apiKey || path === "/admin/api/setup") return;
    fetch("/admin/api/client-events", {
      method: "POST",
      headers: {"Authorization": "Bearer " + state.apiKey, "Content-Type": "application/json",
        "X-Workbuddy-Admin": "1", "X-Trace-ID": trace},
      body: JSON.stringify({event: event, route: path.split("?")[0], status: status})
    }).catch(function () { /* 原请求失败仍由服务端审计记录，不递归发送失败事件。 */ });
  }

  function lockDashboard() {
    $("tabs").classList.add("hidden");
    $("dashboard").classList.add("hidden");
    state.lastStatus = null;
    ["meta", "summaryCards", "siteSummary", "gatewayInfo", "logBox", "configBox"].forEach(function (id) {
      $(id).textContent = "";
    });
    ["accountsTable", "modelsTable", "credentialsTable"].forEach(function (id) {
      $(id).querySelector("tbody").innerHTML = "";
    });
  }

  function api(path, options) {
    options = options || {};
    var trace = traceID();
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
        showAlert("HTTP " + resp.status + "：" + (result.data.error || "请求失败") + "；已有数据保留，未更新为零。");
        throw new Error("HTTP " + resp.status);
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
    ["失效", "disabled", "danger"]
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
      kvRow("只读管理台", g.webuiEnabled ? "已启用" : "未启用") +
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

  function renderAccounts(list) {
    var tb = $("accountsTable").querySelector("tbody");
    if (!list.length) {
      tb.innerHTML = '<tr><td colspan="15" class="empty">暂无账号数据（服务是否已写入状态快照？）</td></tr>';
      return;
    }
    tb.innerHTML = list.map(function (a, i) {
      var st = STATE_TEXT[a.state] || a.state || "-";
      var cls = STATE_CLASS[a.state] || "";
      var site = a.edition === "intl" ? "国际站" : "国内站";
      return "<tr>" +
        "<td>" + (i + 1) + "</td>" +
        "<td class='mono'>" + esc(a.path) + "</td>" +
        "<td>" + esc(a.nickname || "-") + "</td>" +
        "<td>" + esc(site) + "</td>" +
        "<td><span class='badge " + cls + "'>" + esc(st) + "</span></td>" +
        "<td>" + esc(fmtTime(a.tokenExpiresAt)) + "</td>" +
        "<td>" + esc(a.quotaKnown ? count(a.quotaTotal) : "-") + "</td>" +
        "<td>" + esc(a.quotaKnown ? count(a.quotaUsed) : "-") + "</td>" +
        "<td>" + esc(a.quotaKnown ? count(a.quotaRemaining) : "-") + "</td>" +
        "<td>" + esc((a.planLabel || "-") + (a.planStale ? "（待刷新）" : "")) + "</td>" +
        "<td>" + esc(count(a.freeModels)) + "</td>" +
        "<td>" + esc(count(a.modelCooldowns)) + "</td>" +
        "<td>" + esc(fmtTime(a.refreshExpiresAt)) + "</td>" +
        "<td>" + esc(fmtTime(a.lastRefreshTime)) + "</td>" +
        "<td>" + esc(count(a.refreshFailCount)) + "</td>" +
        "</tr>";
    }).join("");
  }

  // ---- 渲染：模型 --------------------------------------------------------
  function renderModels(data) {
    var list = data.models || [];
    var onlyUsable = $("onlyUsable").checked;
    if (onlyUsable) {
      list = list.filter(function (m) { return (m.availableAccounts || 0) > 0; });
    }
    var tb = $("modelsTable").querySelector("tbody");
    if (!list.length) {
      tb.innerHTML = '<tr><td colspan="9" class="empty">暂无模型数据</td></tr>';
      return;
    }
    tb.innerHTML = list.map(function (m) {
      return "<tr>" +
        "<td class='mono'>" + esc(m.id) + "</td>" +
        "<td>" + esc(m.cnMultiplier || "-") + "</td>" +
        "<td>" + esc(m.intlMultiplier || "-") + "</td>" +
        "<td>" + esc(count(m.availableAccounts)) + "</td>" +
        "<td>" + esc(count(m.requests)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgTtftMs, m.hasTtft)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgLatencyMs, m.hasLatency)) + "</td>" +
        "<td>" + esc(fmtTokensM(m.tokens)) + "</td>" +
        "<td>" + esc(m.lastStatus || "-") + "</td>" +
        "</tr>";
    }).join("");
    if (data.stale) showAlert("模型统计快照过旧，当前展示的是上次数据。");
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
    });
  }

  // ---- 渲染：配置 --------------------------------------------------------
  function loadConfig() {
    return api("/admin/api/config").then(function (data) {
      var box = $("configBox");
      if (!data.exists) { box.textContent = "未找到 " + (data.path || "config.json"); return; }
      box.textContent = $("maskHints").checked ? maskSystemPrompt(data.content) : data.content;
    }).catch(function () {
      // 保留上次内容。
    });
  }

  // maskSystemPrompt 隐藏 systemPrompt 正文字符数，避免把提示词内容展示在浏览器里。
  function maskSystemPrompt(text) {
    try {
      var obj = JSON.parse(text);
      if (obj && obj.systemPrompt && typeof obj.systemPrompt === "object") {
        ["fallback", "force"].forEach(function (k) {
          if (typeof obj.systemPrompt[k] === "string" && obj.systemPrompt[k].trim() !== "") {
            obj.systemPrompt[k] = "（已隐藏，长度 " + obj.systemPrompt[k].length + " 字符）";
          }
        });
      }
      return JSON.stringify(obj, null, 2);
    } catch (e) {
      return text;
    }
  }

  // ---- 渲染：凭据 --------------------------------------------------------
  function loadCredentials() {
    return api("/admin/api/credentials").then(function (data) {
      $("credHint").textContent = data.hint || "";
      var tb = $("credentialsTable").querySelector("tbody");
      var list = data.files || [];
      if (!list.length) {
        tb.innerHTML = '<tr><td colspan="10" class="empty">未发现凭据文件</td></tr>';
        return;
      }
      tb.innerHTML = list.map(function (c) {
        var site = c.edition === "intl" ? "国际站" : "国内站";
        if (!c.exists) {
          return "<tr><td class='mono'>" + esc(c.path) + "</td><td colspan='9' class='empty'>文件不存在</td></tr>";
        }
        if (c.error) {
          return "<tr><td class='mono'>" + esc(c.path) + "</td><td colspan='9' class='empty'>" + esc(c.error) + "</td></tr>";
        }
        return "<tr>" +
          "<td class='mono'>" + esc(c.path) + "</td>" +
          "<td>" + esc(site) + "</td>" +
          "<td>" + esc(c.nickname || "-") + "</td>" +
          "<td class='mono'>" + esc(c.uid || "-") + "</td>" +
          "<td>" + esc(c.domain || "-") + "</td>" +
          "<td class='mono'>" + esc(c.accessTokenMasked || "-") + "</td>" +
          "<td class='mono'>" + esc(c.refreshTokenMasked || "-") + "</td>" +
          "<td>" + esc(fmtTime(c.expiresAt)) + "</td>" +
          "<td>" + esc(fmtTime(c.refreshExpiresAt)) + "</td>" +
          "<td>" + esc(fmtTime(c.lastRefreshTime)) + "</td>" +
          "</tr>";
      }).join("");
    }).catch(function () {
      // 保留上次内容；错误由常驻提示区展示。
    });
  }

  // ---- 路由与轮询 --------------------------------------------------------
  function refreshActive() {
    if (!state.apiKey || state.refreshPending) return Promise.resolve();
    state.refreshPending = true;
    var onErr = function () {
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("连接失败", "danger");
      }
    };
    var pending = Promise.resolve();
    if (state.activeTab === "overview" || state.activeTab === "accounts") {
      pending = api("/admin/api/status").then(renderStatus).catch(onErr);
    } else if (state.activeTab === "models") {
      pending = api("/admin/api/models").then(renderModels).catch(onErr);
    } else if (state.activeTab === "logs") {
      pending = loadLogs(false);
    }
    return pending.then(function () { state.refreshPending = false; }, function () { state.refreshPending = false; });
  }

  function setTab(name) {
    state.activeTab = name;
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.classList.toggle("active", t.getAttribute("data-tab") === name);
    });
    Array.prototype.forEach.call(document.querySelectorAll(".panel"), function (p) {
      p.classList.toggle("active", p.id === "panel-" + name);
    });
    if (name === "logs") loadLogs(true);
    else if (name === "config") loadConfig();
    else if (name === "credentials") loadCredentials();
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
  }

  function loadSettings() {
    return api("/admin/api/settings").then(renderSettings).catch(function () {});
  }

  function saveAPISettings() {
    var enabled = $("apiKeyEnabled").checked;
    var key = $("modelAPIKey").value;
    if (enabled && !key && !(state.settings && state.settings.apiKeyConfigured)) {
      showAlert("开启 API Key 校验前必须输入一个 Key。");
      reportUIEvent("settings_validation_failed", "/admin/api/settings", 0, traceID());
      return;
    }
    $("saveAPISettings").disabled = true;
    return api("/admin/api/settings", {method: "POST", body: {apiKeyEnabled: enabled, apiKey: key}})
      .then(function (data) {
        renderSettings(data);
        showAlert("已写入 config.json，API 鉴权设置立即生效；管理鉴权未改变。", true);
      }).catch(function () {}).then(function () { $("saveAPISettings").disabled = false; });
  }

  // 管理 Key 仅保存在当前标签页会话，验证成功后才保存。
  function saveKey() {
    var key = $("apiKey").value.trim();
    if (!key) { showAlert("请输入管理 Key。"); return Promise.resolve(); }
    state.apiKey = key;
    setConn("校验中…", "");
    return api("/admin/api/settings").then(function (data) {
      sessionStorage.setItem(KEY_STORAGE, key);
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
            "③ 直接在服务器上编辑 config.json，把 gateway.adminKey 设为随机字符串后重启网关。");
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
    $("apiKey").value = state.apiKey;
    initTheme();
    $("saveKey").addEventListener("click", saveKey);
    $("logout").addEventListener("click", function () {
      sessionStorage.removeItem(KEY_STORAGE);
      state.apiKey = "";
      window.location.reload();
    });
    $("initializeAdmin").addEventListener("click", initializeAdmin);
    $("saveAPISettings").addEventListener("click", saveAPISettings);
    $("apiKey").addEventListener("keydown", function (e) { if (e.key === "Enter") saveKey(); });
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.addEventListener("click", function () { setTab(t.getAttribute("data-tab")); });
    });
    $("onlyUsable").addEventListener("change", refreshActive);
    $("logFile").addEventListener("change", function () { loadLogs(false); });
    $("logLines").addEventListener("change", function () { loadLogs(false); });
    $("maskHints").addEventListener("change", loadConfig);

    discoverSetup();
    state.timer = setInterval(refreshActive, POLL_MS);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
