(function () {
  "use strict";

  var KEY_STORAGE = "wb_gateway_api_key";
  var POLL_MS = 3000;

  var state = {
    apiKey: localStorage.getItem(KEY_STORAGE) || "",
    activeTab: "overview",
    lastStatus: null,
    timer: null
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

  function api(path) {
    var headers = {};
    if (state.apiKey) headers["Authorization"] = "Bearer " + state.apiKey;
    return fetch(path, { headers: headers }).then(function (resp) {
      if (resp.status === 401) {
        setConn("鉴权失败 (401)", "danger");
        showAlert("鉴权失败（401）：请确认服务是用同一个密钥启动的，例如 serve -webui -api-key sk-demo。");
        throw new Error("unauthorized");
      }
      if (!resp.ok) {
        setConn("请求失败 (" + resp.status + ")", "danger");
        showAlert("请求失败：HTTP " + resp.status + "（" + path + "）");
        throw new Error("HTTP " + resp.status);
      }
      hideAlert();
      setConn("已连接 · " + nowText(), "ok");
      return resp.json();
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
        "<td>" + esc(num(a.quotaTotal)) + "</td>" +
        "<td>" + esc(num(a.quotaUsed)) + "</td>" +
        "<td>" + esc(num(a.quotaRemaining)) + "</td>" +
        "<td>" + esc(a.planLabel || "-") + "</td>" +
        "<td>" + esc(num(a.freeModels)) + "</td>" +
        "<td>" + esc(num(a.modelCooldowns)) + "</td>" +
        "<td>" + esc(fmtTime(a.refreshExpiresAt)) + "</td>" +
        "<td>" + esc(fmtTime(a.lastRefreshTime)) + "</td>" +
        "<td>" + esc(num(a.refreshFailCount)) + "</td>" +
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
        "<td>" + esc(num(m.availableAccounts)) + "</td>" +
        "<td>" + esc(num(m.requests)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgTtftMs, m.hasTtft)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgLatencyMs, m.hasLatency)) + "</td>" +
        "<td>" + esc(fmtTokensM(m.tokens)) + "</td>" +
        "<td>" + esc(m.lastStatus || "-") + "</td>" +
        "</tr>";
    }).join("");
  }

  // ---- 渲染：日志 --------------------------------------------------------
  function loadLogs(initial) {
    var file = initial ? "" : $("logFile").value;
    var lines = $("logLines").value;
    var q = "/admin/api/logs?lines=" + encodeURIComponent(lines);
    if (file) q += "&file=" + encodeURIComponent(file);
    api(q).then(function (data) {
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
    }).catch(function () {
      $("logBox").textContent = "读取日志失败";
    });
  }

  // ---- 渲染：配置 --------------------------------------------------------
  function loadConfig() {
    api("/admin/api/config").then(function (data) {
      var box = $("configBox");
      if (!data.exists) { box.textContent = "未找到 " + (data.path || "config.json"); return; }
      box.textContent = $("maskHints").checked ? maskSystemPrompt(data.content) : data.content;
    }).catch(function () {
      $("configBox").textContent = "读取配置失败";
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
    api("/admin/api/credentials").then(function (data) {
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
      $("credentialsTable").querySelector("tbody").innerHTML =
        '<tr><td colspan="10" class="empty">读取凭据失败</td></tr>';
    });
  }

  // ---- 路由与轮询 --------------------------------------------------------
  function refreshActive() {
    var onErr = function () {
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("连接失败", "danger");
      }
    };
    if (state.activeTab === "overview" || state.activeTab === "accounts") {
      api("/admin/api/status").then(renderStatus).catch(onErr);
    } else if (state.activeTab === "models") {
      api("/admin/api/models").then(renderModels).catch(onErr);
    } else if (state.activeTab === "logs") {
      loadLogs(false);
    }
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
    else refreshActive();
  }

  // saveKey 保存后立即发一次真实请求校验，并把结果明确告知用户（成功/401/连接失败）。
  function saveKey() {
    var key = $("apiKey").value.trim();
    state.apiKey = key;
    localStorage.setItem(KEY_STORAGE, key);
    setConn("校验中…", "");
    showAlert("已保存，正在校验密钥…", true);
    api("/admin/api/status").then(function (data) {
      renderStatus(data);
      showAlert("已保存，鉴权通过。", true);
    }).catch(function () {
      // api() 已提示 401 / HTTP 错误；这里补充网络类错误。
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("连接失败", "danger");
        showAlert("已保存，但无法连接网关：请检查访问地址与端口是否为当前 serve 实例。");
      }
    });
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
    $("apiKey").value = state.apiKey;
    initTheme();
    $("saveKey").addEventListener("click", saveKey);
    $("apiKey").addEventListener("keydown", function (e) { if (e.key === "Enter") saveKey(); });
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.addEventListener("click", function () { setTab(t.getAttribute("data-tab")); });
    });
    $("onlyUsable").addEventListener("change", refreshActive);
    $("logFile").addEventListener("change", function () { loadLogs(false); });
    $("logLines").addEventListener("change", function () { loadLogs(false); });
    $("maskHints").addEventListener("change", loadConfig);

    if (!state.apiKey) {
      setConn("未设置密钥", "");
      showAlert("请先填写与网关 -api-key 一致的 API 密钥并保存，管理台接口需要鉴权。");
    }
    setTab("overview");
    state.timer = setInterval(refreshActive, POLL_MS);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();