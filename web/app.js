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
    alertSource: "",
    alertKind: "",
    alertTimer: null,
    alertVersion: 0,
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
    rateRevision: null,
    openRateKeys: {},
    openRawLogs: {},
    statsPending: false,
    statsVersion: 0,
    statsLastPoll: 0,
    lastStats: null,
    statsRenderKey: "",
    accountAlertsPending: false,
    accountAlertsLastPoll: 0,
    accountAlertsGeneration: 0,
    lastAccountAlerts: null,
    addAccountId: "",
    addAccountPoll: 0,
    addAccountDeadline: 0,
    statsAccount: "",
    statsDatesTouched: false,
    redactDirty: false,
    redactSettings: null,
    lastRedact: null,
    redactVersion: 0,
    redactDetailVersion: 0,
    redactSelected: "",
    redactDetail: null,
    redactRevealed: null,
    redactLimit: 50
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

  function showAlert(msg, ok, source) {
    if (state.alertTimer !== null) clearTimeout(state.alertTimer);
    var version = ++state.alertVersion;
    state.alertSource = source || "";
    state.alertKind = ok ? "success" : "error";
    state.alertTimer = null;
    var el = $("alert");
    el.textContent = msg;
    el.className = "alert" + (ok ? " ok" : "");
    // Successful actions stay readable instead of disappearing on the next poll.
    // Errors remain until their own endpoint recovers or a newer action replaces them.
    if (ok) state.alertTimer = setTimeout(function () {
      if (version === state.alertVersion) hideAlert();
    }, 5000);
  }

  function hideAlert() {
    if (state.alertTimer !== null) clearTimeout(state.alertTimer);
    state.alertTimer = null;
    state.alertVersion++;
    state.alertSource = state.alertKind = "";
    $("alert").className = "alert hidden";
  }

  function setHTMLIfChanged(el, html) {
    if (el._sourceHTML === html && (el.childNodes ? el.childNodes.length : el.innerHTML === html)) return false;
    el.innerHTML = html;
    el._sourceHTML = html;
    return true;
  }

  // Keep stable rows/cells, focus and expanded details during background polling.
  // Only renderers' source markup is compared, not transient detail-panel content.
  function updateTableBody(table, html) {
    if (table._tableSourceHTML === html && (table.children ? table.children.length : table.innerHTML === html)) return;
    if (!document.createElement || !table.children) {
      table.innerHTML = html;
      table._tableSourceHTML = html;
      return;
    }
    var template = document.createElement("template");
    template.innerHTML = html;
    var rows = Array.prototype.slice.call(template.content.children);
    var wanted = Object.create(null), existing = Object.create(null);
    var keyed = rows.every(function (row) {
      var key = row.getAttribute("data-row-key");
      if (key === null || wanted[key]) return false;
      wanted[key] = true;
      return true;
    });
    if (!keyed) {
      table.innerHTML = html;
      table._tableSourceHTML = html;
      return;
    }
    Array.prototype.forEach.call(table.children, function (row) {
      var key = row.getAttribute("data-row-key");
      if (key !== null) existing[key] = row;
    });
    rows.forEach(function (candidate, index) {
      var key = candidate.getAttribute("data-row-key");
      var row = existing[key], source = candidate.outerHTML;
      if (!row) {
        row = candidate;
        Array.prototype.forEach.call(row.cells, function (cell) { cell._sourceHTML = cell.outerHTML; });
      } else if (row._sourceHTML !== source) {
        if (row.cells.length !== candidate.cells.length) {
          row.replaceWith(candidate);
          row = candidate;
          Array.prototype.forEach.call(row.cells, function (cell) { cell._sourceHTML = cell.outerHTML; });
        } else {
          Array.prototype.slice.call(candidate.cells).forEach(function (cell, i) {
            if (row.cells[i]._sourceHTML !== cell.outerHTML) {
              cell._sourceHTML = cell.outerHTML;
              row.replaceChild(cell, row.cells[i]);
            }
          });
        }
      }
      row._sourceHTML = source;
      var before = table.children[index];
      if (before !== row) table.insertBefore(row, before || null);
    });
    Array.prototype.slice.call(table.children).forEach(function (row) {
      var key = row.getAttribute("data-row-key");
      if (key === null || !wanted[key]) row.remove();
    });
    table._tableSourceHTML = html;
  }

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
    resetRedactPage();
    state.tasksLastPoll = 0;
    $("tabs").classList.add("hidden");
    $("dashboard").classList.add("hidden");
    state.lastStatus = null;
    state.lastModels = null;
    state.rateDetails = null;
    state.rateDetailsAt = 0;
    state.rateDetailsPending = null;
    state.rateRevision = null;
    state.openRateKeys = {};
    state.openRawLogs = {};
    state.statsVersion++;
    state.statsLastPoll = 0;
    state.lastStats = null;
    state.statsRenderKey = "";
    hideAllStatsChartHover();
    statsChartControllers = [];
    ["usageSummary", "usageChart", "latencyChart", "statsInfo", "statsRange"].forEach(function (id) {
      $(id).textContent = "";
      $(id)._sourceHTML = "";
    });
    if ($("latencyLegend")) {
      $("latencyLegend").textContent = "";
      $("latencyLegend")._sourceHTML = "";
    }
    $("statsAccount").innerHTML = '<option value="">全局 · 全部账号</option>';
    $("statsAccount")._sourceHTML = "";
    $("statsTable").querySelector("tbody").innerHTML = "";
    $("statsTable").querySelector("tbody")._sourceHTML = "";
    state.accountAlertsGeneration++;
    state.accountAlertsLastPoll = 0;
    state.lastAccountAlerts = null;
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    $("poolHealth").textContent = "—";
    $("poolHealth").className = "health-value";
    $("poolHealthBar").style.width = "0%";
    $("poolHealthStatus").textContent = "等待账号状态";
    $("accountAlertsInfo").textContent = "登录后读取账号异常日志。";
    $("accountAlerts").innerHTML = '<p class="stats-empty">等待日志</p>';
    $("accountAlerts")._alertSourceHTML = null;
    if ($("accountAlertTooltip")) $("accountAlertTooltip").hidden = true;
    ["meta", "gatewayInfo", "logBox", "configBox", "tasksInfo"].forEach(function (id) {
      $(id).textContent = "";
      $(id)._sourceHTML = "";
    });
    ["accountsTable", "modelsTable", "tasksTable"].forEach(function (id) {
      var body = $(id).querySelector("tbody");
      body.innerHTML = "";
      body._tableSourceHTML = "";
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
        if (!options.quiet) {
          setConn("请求失败 (" + resp.status + ")", "danger");
          showAlert("HTTP " + resp.status + "：" + (result.data.error || "请求失败") +
            (path.split("?")[0] === "/admin/api/config-edit" ? "；操作未完成，已有编辑输入保留。" : "；已有数据保留，未更新为零。"),
            false, path.split("?")[0]);
        }
        var failure = new Error("HTTP " + resp.status);
        failure.status = resp.status;
        failure.detail = result.data.error || "请求失败";
        throw failure;
      }
      if (state.alertKind === "error" && state.alertSource === path.split("?")[0]) hideAlert();
      setConn("已连接 · " + nowText(), "ok");
      reportUIEvent("request_completed", path, status, trace);
      return result.data;
    }).catch(function (err) {
      if (keyAtStart === state.apiKey) {
        reportUIEvent("request_failed", path, status, trace);
        if (!status && !options.quiet) {
          setConn("连接失败", "danger");
          showAlert("请求未完成；保留上次数据，请检查网络后重试。", false, path.split("?")[0]);
        }
      }
      throw err;
    });
  }

  // ---- 渲染：总览 --------------------------------------------------------
  function renderPoolHealth(data) {
    var h = data.health || {}, p = h.percent;
    var known = !data.stale && typeof p === "number" && isFinite(p) && p >= 0 && p <= 100;
    var stateClass = known ? (p === 100 ? "healthy" : (p === 0 ? "unavailable" : "degraded")) : "unknown";
    $("poolHealth").textContent = known ? p.toLocaleString("zh-CN", {maximumFractionDigits: 1}) + "%" : "—";
    $("poolHealth").className = "health-value " + stateClass;
    $("poolHealthBar").style.width = known ? p + "%" : "0%";
    $("poolHealthBar").className = stateClass;
    $("poolHealthStatus").textContent = data.stale ? "快照过旧 · 健康度待确认" :
      h.state === "empty" ? "未加载账号 · 暂无健康度" : !known ? "健康度待确认" :
      p === 100 ? "账号池全部可用" : p === 0 ? "账号池当前不可用，请查看告警" : "账号池部分受限，请查看告警";
  }

  function renderAccountAlerts(data) {
    state.lastAccountAlerts = data;
    var labels = {urgent: "紧急", important: "重要", warning: "注意", notice: "提示"};
    var alerts = data.alerts || [];
    var snapshot = state.lastStatus;
    // 日志扫描短缓存期间仍用最新账号快照剔除已恢复项，不把历史故障继续当作当前告警。
    if (snapshot && !snapshot.stale && Array.isArray(snapshot.accounts)) {
      alerts = alerts.filter(function (a) {
        return snapshot.accounts.some(function (acc) {
          return acc.path === a.account && (a.state === "paused" ? acc.schedulingEnabled === false :
            a.state === (acc.state === "quota_exhausted" ? "paid_exhausted" : acc.state));
        });
      });
    }
    var notes = [];
    if (data.stale || (snapshot && snapshot.stale)) notes.push("快照过旧，以下为上次确认的异常");
    if (data.missingLogCount) notes.push("部分异常暂未找到对应日志，可能等待下一次扫描");
    if (data.scanTruncated || data.listTruncated) notes.push("已达到读取上限，仅显示扫描范围内记录");
    $("accountAlertsInfo").textContent = notes.length ? notes.join("；") + "。" : "";
    var html = alerts.length ? alerts.map(function (a) {
      var level = labels[a.severity] ? a.severity : "notice";
      var detail = labels[level] + " · " + a.label + " · " + a.account +
        (a.at ? " · " + fmtTime(a.at) : "") + (a.message ? "\n" + a.message : "");
      return '<article class="account-alert alert-' + level + '" tabindex="0" data-alert-detail="' +
        esc(detail) + '" aria-label="' + esc(detail) + '"><div class="account-alert-head">' +
        '<span class="alert-priority">' + esc(labels[level]) + '</span><strong>' + esc(a.label) +
        '</strong><span class="account-alert-file">' + esc(a.account) + '</span><time>' + esc(fmtTime(a.at)) +
        '</time></div></article>';
    }).join("") : '<p class="stats-empty">' + (data.missingLogCount ? "当前存在异常，但扫描范围内暂未找到对应日志" :
      data.stale ? "暂无日志记录，账号状态待确认" : "未发现当前账号异常日志") + '</p>';
    // Unchanged polling must not replace a hovered/focused row or dismiss its details.
    if ($("accountAlerts")._alertSourceHTML !== html) {
      if ($("accountAlertTooltip")) $("accountAlertTooltip").hidden = true;
      $("accountAlerts").innerHTML = html;
      $("accountAlerts")._alertSourceHTML = html;
    }
  }

  function bindAccountAlertTooltip() {
    var list = $("accountAlerts"), tip = $("accountAlertTooltip");
    if (!list || !tip || !list.addEventListener) return;
    var pinned = null;
    var hide = function () {
      pinned = null;
      tip.hidden = true;
    };
    var show = function (target, x, y) {
      if (!target) return;
      var detail = target.getAttribute("data-alert-detail") || "";
      if (!detail) return;
      tip.textContent = detail;
      tip.hidden = false;
      var width = tip.offsetWidth || 260, height = tip.offsetHeight || 80;
      var left = Math.max(8, Math.min(x + 14, window.innerWidth - width - 8));
      var top = y + 14;
      if (top + height > window.innerHeight - 8) top = y - height - 14;
      tip.style.left = left + "px";
      tip.style.top = Math.max(8, top) + "px";
    };
    list.addEventListener("pointermove", function (event) {
      if (event.pointerType === "touch") return;
      var target = event.target && event.target.closest ? event.target.closest(".account-alert") : null;
      if (!target) { hide(); return; }
      show(target, event.clientX, event.clientY);
    });
    // Touch emits pointerleave after every tap; it is not a hover exit.
    list.addEventListener("pointerleave", function (event) {
      if (event.pointerType !== "touch") hide();
    });
    list.addEventListener("pointercancel", hide);
    list.addEventListener("scroll", hide, {passive: true});
    list.addEventListener("focusin", function (event) {
      var target = event.target && event.target.closest ? event.target.closest(".account-alert") : null;
      if (!target) return;
      var rect = target.getBoundingClientRect ? target.getBoundingClientRect() : {left: 0, bottom: 0};
      show(target, rect.left, rect.bottom);
    });
    list.addEventListener("focusout", hide);
    list.addEventListener("click", function (event) {
      var target = event.target && event.target.closest ? event.target.closest(".account-alert") : null;
      if (!target) return;
      var rect = target.getBoundingClientRect ? target.getBoundingClientRect() : {left: 0, bottom: 0};
      // Focus/hover can already have shown the tooltip before click fires.
      // Track explicit clicks separately, so a first tap never immediately hides it.
      if (pinned === target && !tip.hidden) hide();
      else { pinned = target; show(target, rect.left, rect.bottom); }
    });
    window.addEventListener("blur", hide);
  }

  function loadAccountAlerts() {
    if (!state.apiKey || state.accountAlertsPending || Date.now() - state.accountAlertsLastPoll < 10000) return Promise.resolve();
    var generation = state.accountAlertsGeneration, key = state.apiKey;
    state.accountAlertsPending = true;
    state.accountAlertsLastPoll = Date.now();
    return api("/admin/api/account-alerts").then(function (data) {
      if (generation === state.accountAlertsGeneration && key === state.apiKey) renderAccountAlerts(data);
    }).catch(function () {
      if (generation === state.accountAlertsGeneration && key === state.apiKey) {
        $("accountAlertsInfo").textContent = "告警日志读取失败，保留上次结果；这不代表账号池没有异常。";
      }
    }).then(function () { state.accountAlertsPending = false; });
  }

  // 统计事实来自可选 stats.json；所有查询使用管理鉴权和既有前端落盘审计。
  function statsNumber(v) {
    return Number(v || 0).toLocaleString("zh-CN", {maximumFractionDigits: 2});
  }

  function statsCredit(v) {
    return Number(v || 0).toLocaleString("zh-CN", {maximumFractionDigits: 4});
  }

  function statsDuration(v) {
    return v === null || v === undefined ? "—" : (Number(v) / 1000).toFixed(2) + " s";
  }

  function statsCard(label, value, detail, color) {
    return '<article class="usage-metric ' + color + '"><div class="usage-label">' + esc(label) +
      '</div><div class="usage-value">' + esc(value) + '</div><div class="usage-detail">' + esc(detail) + '</div></article>';
  }

  function statsChartNow(value) {
    return typeof value === "number" && isFinite(value) && value > 0 ? value : Date.now();
  }

  function statsBucketIsFuture(bucket, now) {
    // Bucket start is an absolute Unix millisecond timestamp, not the displayed
    // MM-DD label. This works across years and browser time zones.
    return typeof bucket.start === "number" && isFinite(bucket.start) && bucket.start > now;
  }

  function statsChartValue(series, bucket, index, now) {
    if (statsBucketIsFuture(bucket, statsChartNow(now))) return null;
    var value = series.value(bucket, index);
    return value === null || value === undefined || !isFinite(value) ? null : Number(value);
  }

  function statsChartAxes(buckets, opts, cutoff) {
    var now = statsChartNow(cutoff === undefined ? opts.nowMs : cutoff);
    var lines = [], bars = [];
    (opts.lines || []).forEach(function (series) { buckets.forEach(function (b, i) {
      if (statsBucketIsFuture(b, now)) return;
      var v = statsChartValue(series, b, i, now);
      if (v !== null || series.connectZero) lines.push(v === null ? 0 : v);
    }); });
    (opts.bars || []).forEach(function (series) { buckets.forEach(function (b, i) {
      var v = statsChartValue(series, b, i, now);
      if (v !== null) bars.push(v);
    }); });
    var dual = !!opts.rightFormat && bars.length > 0;
    var scaleMax = function (values) {
      var maximum = 0;
      values.forEach(function (v) { maximum = Math.max(maximum, v); });
      return (maximum > 0 ? maximum : 1) * 1.12;
    };
    var leftMax = scaleMax(dual ? lines : lines.concat(bars)), rightMax = scaleMax(dual ? bars : []);
    var ticks = function (format, maximum) {
      return [0, 1, 2, 3, 4].map(function (n) { return String(format(maximum * (1 - n / 4))); });
    };
    var leftTicks = ticks(opts.leftFormat || String, leftMax);
    var rightTicks = dual ? ticks(opts.rightFormat, rightMax) : [];
    var padding = function (labels, minimum) {
      return Math.min(130, Math.max(minimum, 14 + Math.max.apply(null, labels.map(function (t) { return t.length; })) * 7));
    };
    return {hasValues: lines.length + bars.length > 0, dual: dual,
      leftMax: leftMax, rightMax: rightMax, leftTicks: leftTicks, rightTicks: rightTicks,
      left: padding(leftTicks, 52), right: dual ? padding(rightTicks, 44) : 16};
  }

  // 原生 SVG，不访问 CDN。折线默认跨过无样本时间桶连接相邻样本；
  // series.connectZero=true 时把无样本桶按 0 画（用量图使用）。
  // 柱形仍只在有样本的桶出现；opts = {lines, bars, title, leftFormat, rightFormat, box}。
  function statsComboChart(buckets, opts) {
    opts = opts || {};
    var lines = opts.lines || [], bars = opts.bars || [];
    var title = opts.title || "";
    var leftFormat = opts.leftFormat || function (v) { return String(v); };
    var rightFormat = opts.rightFormat || null;
    var box = opts.box || {w: 900, h: 260};
    var now = statsChartNow(opts.nowMs);
    var pick = function (s, b, i) { return statsChartValue(s, b, i, now); };
    var lineValue = function (s, b, i) {
      if (statsBucketIsFuture(b, now)) return null;
      var value = pick(s, b, i);
      return value === null && s.connectZero ? 0 : value;
    };
    var axes = statsChartAxes(buckets, opts, now);
    if (!axes.hasValues) {
      var futureOnly = buckets.length && buckets.every(function (b) { return statsBucketIsFuture(b, now); });
      return '<p class="stats-empty">' + (futureOnly ? "所选时间范围尚未开始" : "该时间范围暂无可用样本") + '</p>';
    }
    var dual = axes.dual, leftMax = axes.leftMax, rightMax = axes.rightMax;
    var width = Math.max(260, Math.round(box.w)), height = Math.max(190, Math.round(box.h));
    var top = 18, bottom = 40;
    var leftTicks = axes.leftTicks, rightTicks = axes.rightTicks;
    var left = axes.left, right = axes.right;
    // 两张图共用同一组边距时，日期刻度和十字线才能逐像素对齐。
    if (opts.plotPad) {
      if (opts.plotPad.left != null) left = opts.plotPad.left;
      if (opts.plotPad.right != null) right = opts.plotPad.right;
    }
    var plotW = width - left - right, plotH = height - top - bottom;
    var x = function (i) { return buckets.length < 2 ? left + plotW / 2 : left + i * plotW / (buckets.length - 1); };
    var yLeft = function (v) { return top + plotH * (1 - v / leftMax); };
    var yRight = function (v) { return top + plotH * (1 - v / rightMax); };
    var out = '<div class="chart-canvas"><svg viewBox="0 0 ' + width + ' ' + height + '" role="img" aria-label="' + esc(title) +
      '" data-chart-model=""><title>' + esc(title) + '</title>';
    for (var n = 0; n <= 4; n++) {
      var yy = top + plotH * n / 4;
      out += '<line class="chart-grid" x1="' + left + '" y1="' + yy + '" x2="' + (width - right) + '" y2="' + yy + '"/>' +
        '<text class="chart-label" text-anchor="end" x="' + (left - 8) + '" y="' + (yy + 4) + '">' + esc(leftTicks[n]) + '</text>';
      if (dual) {
        out += '<text class="chart-label" text-anchor="start" x="' + (width - right + 8) + '" y="' + (yy + 4) + '">' + esc(rightTicks[n]) + '</text>';
      }
    }
    // 窄屏把日期缩成 MM-DD；刻度数量按可用宽度反推，避免标签互相重叠。
    var narrow = width < 520;
    var tickLabel = function (text) {
      return narrow && /^\d{4}-\d{2}-\d{2}$/.test(text) ? text.slice(5) : text;
    };
    var widest = 0;
    buckets.forEach(function (b) { widest = Math.max(widest, tickLabel(b.label).length); });
    var perTick = Math.max(44, widest * 7 + 14);
    var maxTicks = Math.max(2, Math.floor(plotW / perTick));
    var every = Math.max(1, Math.ceil(buckets.length / maxTicks));
    // 首尾标签贴边（居中会溢出 viewBox 被裁），再按像素间距贪心取舍：
    // 末尾刻度优先级最高，与前一个冲突时挤掉前一个，保证最后一个日期始终可见。
    var labelW = widest * 6.6;
    var lastIdx = buckets.length - 1;
    var candidates = [];
    for (var ci = 0; ci < buckets.length; ci += every) {
      candidates.push(ci);
    }
    if (candidates[candidates.length - 1] !== lastIdx) {
      candidates.push(lastIdx);
    }
    var geometry = function (i, isLast) {
      var anchor = i === 0 ? "start" : (isLast ? "end" : "middle");
      var tx = i === 0 ? left : (isLast ? width - right : x(i));
      var l = anchor === "start" ? tx : (anchor === "end" ? tx - labelW : tx - labelW / 2);
      return {i: i, anchor: anchor, tx: tx, left: l, right: l + labelW};
    };
    var kept = [];
    candidates.forEach(function (i) {
      var isLast = i === lastIdx, geo = geometry(i, isLast);
      if (isLast) {
        while (kept.length && geo.left - kept[kept.length - 1].right < 8) {
          kept.pop();
        }
      } else if (kept.length && geo.left - kept[kept.length - 1].right < 8) {
        return;
      }
      kept.push(geo);
    });
    kept.forEach(function (t) {
      out += '<text class="chart-label" text-anchor="' + t.anchor + '" x="' + t.tx + '" y="' + (height - 12) + '">' +
        esc(tickLabel(buckets[t.i].label)) + '</text>';
    });
    var model = {width: width, height: height, left: left, top: top, plotW: plotW, plotH: plotH, count: buckets.length, buckets: []};
    if (bars.length) {
      // 柱组必须居中在该桶折线点的时间位置上，否则柱会比线偏半个桶位；
      // 只做 SVG 边界保护，正常尺寸下不会触发。
      var slot = plotW / Math.max(1, buckets.length);
      var groupW = Math.min(slot * 0.7, 24);
      var barW = Math.max(1, groupW / bars.length);
      var baseY = top + plotH;
      buckets.forEach(function (b, i) {
        var groupX = Math.min(Math.max(x(i) - groupW / 2, 0), width - groupW);
        bars.forEach(function (s, j) {
          var value = pick(s, b, i);
          if (value === null) return;
          var yy = (dual ? yRight : yLeft)(value);
          var h = Math.max(1, baseY - yy);
          out += '<rect class="chart-bar series-' + s.color + '" x="' + (groupX + j * barW).toFixed(2) +
            '" y="' + yy.toFixed(2) + '" width="' + Math.max(1, barW - 1).toFixed(2) + '" height="' + h.toFixed(2) + '"></rect>';
        });
      });
    }
    lines.forEach(function (s) {
      var points = [], paths = [], connectGaps = s.connectGaps !== false;
      buckets.forEach(function (b, i) {
        var value = lineValue(s, b, i);
        if (value === null) {
          if (!connectGaps && points.length) {
            paths.push(points.join(" "));
            points = [];
          }
          return;
        }
        var xx = x(i), yy = yLeft(value);
        points.push((points.length ? "L" : "M") + xx.toFixed(2) + "," + yy.toFixed(2));
        out += '<circle class="series-' + s.color + '" cx="' + xx.toFixed(2) + '" cy="' + yy.toFixed(2) +
          '" r="' + (buckets.length > 80 ? 1.8 : 3.2) + '"></circle>';
      });
      if (points.length) paths.push(points.join(" "));
      paths.forEach(function (p) { out += '<path class="chart-line series-' + s.color + '" d="' + p + '"/>'; });
    });
    buckets.forEach(function (b, i) {
      var row = {label: b.label, x: x(i), future: statsBucketIsFuture(b, now), lines: [], bars: []};
      lines.forEach(function (s) {
        var value = lineValue(s, b, i);
        row.lines.push({label: s.label, color: s.color, value: value,
          display: value === null ? "—" : leftFormat(value), y: value === null ? null : yLeft(value)});
      });
      bars.forEach(function (s) {
        var value = pick(s, b, i);
        row.bars.push({label: s.label, color: s.color, value: value,
          display: value === null ? "—" : (dual ? rightFormat : leftFormat)(value),
          y: value === null ? null : (dual ? yRight : yLeft)(value)});
      });
      // Shared overview readout is separate from the model-specific plotted series.
      row.readout = typeof opts.tooltipRows === "function" ? opts.tooltipRows(b) : null;
      if (row.future && row.readout) row.readout = row.readout.map(function (item) {
        return {label: item.label, color: item.color, display: "—"};
      });
      model.buckets.push(row);
    });
    // 十字线、命中点和浮层都由同一份几何模型驱动，避免 SVG 缩放后坐标漂移。
    out += '<line class="chart-crosshair" x1="0" y1="0" x2="0" y2="0" visibility="hidden"/>' +
      '<g class="chart-hover-dots">';
    lines.forEach(function (s) {
      out += '<circle class="chart-hover-dot series-' + s.color + '" r="4" visibility="hidden"/>';
    });
    out += '</g><rect class="chart-hover-area" x="' + left + '" y="' + top + '" width="' + plotW + '" height="' + plotH +
      '" fill="transparent" pointer-events="all"/>';
    out += '</svg><div class="chart-tooltip" hidden></div></div>';
    return out.replace('data-chart-model=""', 'data-chart-model="' + esc(JSON.stringify(model)) + '"');
  }

  function bindStatsChartHover(container) {
    if (!container || !container.querySelector) return;
    var svg = container.querySelector("svg");
    var tooltip = container.querySelector(".chart-tooltip");
    if (!svg || !tooltip || !svg.getAttribute) return;
    var model;
    try { model = JSON.parse(svg.getAttribute("data-chart-model") || "{}"); } catch (_) { return; }
    if (!model.buckets || !model.buckets.length) return;
    var area = svg.querySelector(".chart-hover-area");
    var cross = svg.querySelector(".chart-crosshair");
    var dots = svg.querySelectorAll ? svg.querySelectorAll(".chart-hover-dot") : [];
    if (!area || !cross) return;
    var group = (container.id === "usageChart" || container.id === "latencyChart") ? $("statsCharts") : null;
    var sharedLine = group ? $("statsChartsCrosshair") : null;
    var plottedIndex = -1, tooltipIndex = -1, tooltipSize = null, tooltipHTML = [];
    if (sharedLine) cross.setAttribute("visibility", "hidden");
    var hide = function () {
      cancelStatsChartHover(controller);
      cross.setAttribute("visibility", "hidden");
      Array.prototype.forEach.call(dots, function (dot) { dot.setAttribute("visibility", "hidden"); });
      tooltip.hidden = true;
      plottedIndex = -1;
    };
    var show = function (index, withTooltip, pointer, rect) {
      var row = model.buckets[index];
      if (!row) return;
      // Only the active pane needs a tooltip or a size measurement. Build its
      // content once per bucket, not once per physical pointer event.
      if (withTooltip !== false) {
        if (tooltipIndex !== index) {
          if (!tooltipHTML[index]) {
            var rows = row.readout || row.lines.concat(row.bars);
            tooltipHTML[index] = '<div class="chart-tooltip-date">' + esc(row.label) + '</div>' +
              (row.future ? '<div class="chart-tooltip-status">未到该时间</div>' : '') +
              rows.map(function (item) {
                var dot = item.color ? '<i class="series-dot ' + esc(item.color) + '"></i>' : "";
                return '<div class="chart-tooltip-row"><span class="chart-tooltip-key">' + dot + esc(item.label) +
                  '</span><span class="chart-tooltip-value">' + esc(item.display) + '</span></div>';
              }).join("");
          }
          tooltip.innerHTML = tooltipHTML[index];
          tooltipIndex = index;
          tooltipSize = null;
        }
        if (tooltip.hidden) tooltip.hidden = false;
        // One layout read when content changes. All chart/group rectangles
        // have already been read before this frame starts changing the DOM.
        if (!tooltipSize) tooltipSize = {width: tooltip.offsetWidth || 180, height: tooltip.offsetHeight || 80};
      } else if (!tooltip.hidden) {
        tooltip.hidden = true;
      }
      if (plottedIndex !== index) {
        if (!sharedLine) {
          cross.setAttribute("x1", row.x); cross.setAttribute("x2", row.x);
          cross.setAttribute("y1", model.top); cross.setAttribute("y2", model.top + model.plotH);
          cross.setAttribute("visibility", "visible");
        }
        row.lines.forEach(function (line, i) {
          var dot = dots[i];
          if (!dot) return;
          if (line.value === null) { dot.setAttribute("visibility", "hidden"); return; }
          dot.setAttribute("cx", row.x); dot.setAttribute("cy", line.y);
          dot.setAttribute("visibility", "visible");
        });
        plottedIndex = index;
      }
      if (withTooltip === false) return;
      var scaleX = rect.width ? rect.width / model.width : 1;
      var scaleY = rect.height ? rect.height / model.height : 1;
      var px = pointer && isFinite(pointer.clientX) ? pointer.clientX - rect.left : row.x * scaleX;
      var py = pointer && isFinite(pointer.clientY) ? pointer.clientY - rect.top : (model.top + model.plotH / 2) * scaleY;
      var width = tooltipSize.width, height = tooltipSize.height;
      var left = px + 14;
      if (left + width > rect.width) left = px - width - 14;
      if (left < 4) left = 4;
      var top = py - height / 2;
      if (top < 4) top = 4;
      if (top + height > rect.height) top = rect.height - height - 4;
      tooltip.style.transform = "translate3d(" + left + "px," + Math.max(4, top) + "px,0)";
    };
    if (!group || !sharedLine) {
      area.addEventListener("pointermove", function (event) {
        queueStatsChartHover(controller, event);
      });
    }
    area.addEventListener("pointerleave", function () {
      // Keep the guide through the inter-card gap; the group owns its lifetime.
      if (!group || !sharedLine) hide();
    });
    area.addEventListener("pointercancel", group && sharedLine ? hideAllStatsChartHover : hide);
    var controller = {
      id: container.id, show: show, hide: hide,
      group: group, guide: sharedLine, model: model,
      measure: function () {
        return svg.getBoundingClientRect ? svg.getBoundingClientRect() :
          {left: 0, top: 0, width: model.width, height: model.height};
      },
      position: function (index, rect) {
        var row = model.buckets[index];
        if (!row) return null;
        var scale = rect.height / model.height;
        return {x: rect.left + row.x * rect.width / model.width,
          top: rect.top + model.top * scale, bottom: rect.top + (model.top + model.plotH) * scale};
      }
    };
    if (group && sharedLine) {
      statsChartControllers.push(controller);
      if (!group.statsHoverBound) {
        // The axes, labels, card padding and inter-card gap are part of the
        // linked chart too. Listening only on the transparent plot rect leaves
        // dead zones where the retained guide appears to freeze.
        group.addEventListener("pointermove", function (event) {
          queueStatsChartHover(null, event, group);
        });
        group.addEventListener("pointerleave", hideAllStatsChartHover);
        group.addEventListener("pointercancel", hideAllStatsChartHover);
        group.statsHoverBound = true;
      }
    }
  }

  var statsChartControllers = [];
  var statsChartHoverTrace = "";
  var statsChartHoverFrame = null, statsChartHoverPending = null, statsChartHoverGeneration = 0;

  function cancelStatsChartHover(controller) {
    if (controller && (!statsChartHoverPending || statsChartHoverPending.controller !== controller)) return;
    if (statsChartHoverFrame !== null && window.cancelAnimationFrame) window.cancelAnimationFrame(statsChartHoverFrame);
    statsChartHoverFrame = null;
    statsChartHoverPending = null;
    statsChartHoverGeneration++;
  }

  function queueStatsChartHover(controller, event, group) {
    // Keep only the latest coordinates across BOTH charts. High-polling-rate
    // mice must not generate hundreds of DOM/layout updates between frames.
    if (group && !statsChartControllers.length) return;
    statsChartHoverPending = {controller: controller, group: group, clientX: event.clientX, clientY: event.clientY};
    if (statsChartHoverFrame !== null) return;
    var generation = statsChartHoverGeneration;
    var paint = function () {
      if (generation !== statsChartHoverGeneration) return;
      statsChartHoverFrame = null;
      var pointer = statsChartHoverPending;
      statsChartHoverPending = null;
      if (!pointer) return;
      var source = pointer.controller, rect, frames = null;
      if (pointer.group) {
        // Pick the nearest SVG, including its axes, without reading layout in
        // the physical pointer handler. A gap/title hover uses the nearer pane.
        var nearest = Infinity;
        frames = statsChartControllers.filter(function (c) { return c.group === pointer.group; }).map(function (c) {
          var bounds = c.measure();
          var distance = Math.max(bounds.top - pointer.clientY, pointer.clientY - bounds.top - bounds.height, 0);
          if (!source || distance < nearest) {
            source = c;
            rect = bounds;
            nearest = distance;
          }
          return {controller: c, rect: bounds};
        });
        if (!source) return;
      } else {
        rect = source.measure();
      }
      var model = source.model;
      var viewX = (pointer.clientX - rect.left) * model.width / (rect.width || model.width);
      var index = Math.round((viewX - model.left) / Math.max(1, model.plotW) * (model.count - 1));
      index = Math.max(0, Math.min(model.count - 1, index));
      if (source.group && source.guide) {
        syncStatsChartHover(index, source, source.guide, source.group, pointer, rect, frames);
      } else {
        source.show(index, true, pointer, rect);
      }
    };
    if (window.requestAnimationFrame) statsChartHoverFrame = window.requestAnimationFrame(paint);
    else paint();
  }

  function syncStatsChartHover(index, source, guide, group, pointer, sourceRect, measuredFrames) {
    // Read every rectangle before either pane mutates the DOM, avoiding
    // alternating read/write forced layouts in the linked chart.
    var positions = [], frames = measuredFrames || statsChartControllers.filter(function (c) {
      return c.group === group;
    }).map(function (controller) {
      return {controller: controller, rect: controller === source ? sourceRect : controller.measure()};
    });
    frames.forEach(function (frame) {
      var position = frame.controller.position(index, frame.rect);
      if (position) positions.push(position);
    });
    if (!positions.length) return;
    var bounds = group.getBoundingClientRect();
    var top = Math.min.apply(null, positions.map(function (p) { return p.top; }));
    var bottom = Math.max.apply(null, positions.map(function (p) { return p.bottom; }));
    source.show(index, true, pointer, sourceRect);
    frames.forEach(function (frame) {
      if (frame.controller !== source) frame.controller.show(index, false, null, frame.rect);
    });
    guide.style.transform = "translate3d(" + (positions[0].x - bounds.left) + "px," + (top - bounds.top) + "px,0)";
    var height = (bottom - top) + "px";
    if (guide.style.height !== height) guide.style.height = height;
    guide.hidden = false;
    if (!statsChartHoverTrace) {
      statsChartHoverTrace = traceID();
      reportUIEvent("stats_chart_hover_started", "/admin/api/stats", 0, statsChartHoverTrace);
    }
  }

  function hideAllStatsChartHover() {
    cancelStatsChartHover();
    statsChartControllers.forEach(function (controller) { controller.hide(); });
    var guide = $("statsChartsCrosshair");
    if (guide) guide.hidden = true;
    if (statsChartHoverTrace) {
      reportUIEvent("stats_chart_hover_ended", "/admin/api/stats", 0, statsChartHoverTrace);
      statsChartHoverTrace = "";
    }
  }

  function statsLineChart(buckets, series, title, format, box) {
    return statsComboChart(buckets, {lines: series, bars: [], title: title, leftFormat: format, box: box});
  }

  function renderStatsChart(id, buckets, opts) {
    var el = $(id);
    if (!el) return;
    el.innerHTML = statsComboChart(buckets, opts);
    bindStatsChartHover(el);
  }

  function renderStats(data) {
    state.lastStats = data;
    var s = data.summary || {}, rows = data.buckets || [], store = data.storage || {};
    var chartBox = function (id) {
      var el = $(id), w = el && el.clientWidth ? el.clientWidth : 900;
      return {w: w, h: w < 520 ? 230 : 280};
    };
    var chartWidth = Math.min(chartBox("usageChart").w, chartBox("latencyChart").w);
    var chartNow = statsChartNow(data.serverTime);
    var payload = {};
    Object.keys(data).forEach(function (key) {
      if (key !== "serverTime" && key !== "storage") payload[key] = data[key];
    });
    // A changing server clock alone is not new chart data. Still redraw at a
    // future-bucket boundary, on a filter change, or when the viewport changes.
    var renderKey = JSON.stringify([payload, state.statsAccount, chartWidth,
      rows.map(function (row) { return statsBucketIsFuture(row, chartNow); })]);
    $("statsInfo").textContent = store.message || "";
    if (renderKey === state.statsRenderKey) return;
    var currentTTFT = data.ttftMetric === "first_token_v2";
    var tokenTime = function (row) { return currentTTFT ? row.avgTtftMs : null; };
    $("statsStart").value = data.start;
    $("statsEnd").value = data.end;
    var choice = state.statsAccount || data.account || "";
    var siteScope = choice === "site:cn" || choice === "site:intl";
    var scopeOption = function (value, label) {
      return '<option value="' + esc(value) + '"' + (value === choice ? " selected" : "") + '>' + esc(label) + "</option>";
    };
    setHTMLIfChanged($("statsAccount"), scopeOption("", "全局 · 全部账号") +
      scopeOption("site:intl", "国际站 · 全部国际账号") + scopeOption("site:cn", "国内站 · 全部国内账号") +
      (data.accounts || []).map(function (a) {
        return scopeOption(a.id, a.file + " · " + (a.edition === "intl" ? "国际" : "国内") + " · " + a.id.slice(0, 6));
      }).join(""));
    if (!siteScope && !(data.accounts || []).some(function (a) { return a.id === choice; })) choice = "";
    $("statsAccount").value = choice;
    state.statsAccount = choice;
    var coverage = (s.tokenSamples || 0) + "/" + (s.requests || 0) + " 请求有 Token";
    setHTMLIfChanged($("usageSummary"),
      statsCard("总消耗 Token", s.tokenSamples ? statsNumber(s.tokens) : "—", "输入 " + statsNumber(s.inputTokens) +
        " · 输出 " + statsNumber(s.outputTokens) + " · 缓存读取 " + statsNumber(s.cachedTokens), "token") +
      statsCard("总消耗积分", s.creditSamples ? statsCredit(s.credits) : "—", (s.creditSamples || 0) + "/" + (s.requests || 0) + " 请求有积分 · 仅计 usage.credit", "credit") +
      statsCard("账号调用次数", statsNumber(s.requests), "完整 " + statsNumber(s.success) + " · 失败 / 不完整 " + statsNumber(s.failed), "neutral") +
      statsCard("平均总耗时", statsDuration(s.avgLatencyMs), coverage, "token") +
      statsCard("平均首Token (TTFT)", statsDuration(tokenTime(s)), (currentTTFT ? (s.ttftSamples || 0) : 0) + "/" + (s.requests || 0) + " 请求有新口径有效样本", "first"));
    var sharedBox = {w: chartWidth, h: chartWidth < 520 ? 230 : 280};
    var tooltipRows = function (b) {
      return [
        {label: "总 Token", color: "token", display: b.tokenSamples ? statsNumber(b.tokens) : (b.requests ? "—" : "0")},
        {label: "积分", color: "credit", display: b.creditSamples ? statsCredit(b.credits) : (b.requests ? "—" : "0")},
        {label: "平均总耗时", color: "token", display: statsDuration(b.avgLatencyMs)},
        {label: "平均首Token", color: "first", display: statsDuration(tokenTime(b))}
      ];
    };
    var usageOptions = {
      lines: [{label: "总 Token", color: "token", connectZero: true, value: function (b) { return b.tokenSamples ? b.tokens : null; }}],
      bars: [{label: "积分", color: "credit", value: function (b) { return b.creditSamples ? b.credits : null; }}],
      title: "",
      leftFormat: function (v) { return v >= 1000000 ? (v / 1000000).toFixed(2) + "M" : statsNumber(v); },
      rightFormat: statsCredit,
      box: sharedBox,
      nowMs: chartNow,
      tooltipRows: tooltipRows
    };
    var latencyRows = data.modelLatency || [];
    var latencyLines = [], latencyBars = [];
    latencyRows.forEach(function (row, index) {
      var color = "m" + (index % 8), label = row.model || "未命名模型";
      latencyLines.push({label: label, color: color, value: function (_bucket, i) {
        var points = row.avgLatencyMs || [];
        return i < points.length ? points[i] : null;
      }});
      latencyBars.push({label: label, color: color, value: function (_bucket, i) {
        var points = row.avgTtftMs || [];
        return i < points.length ? points[i] : null;
      }});
    });
    setHTMLIfChanged($("latencyLegend"), latencyLines.map(function (s) {
      return '<i class="series-dot ' + s.color + '"></i><span>' + esc(s.label) + "</span>";
    }).join(""));
    var latencyOptions = {
      lines: latencyLines, bars: latencyBars,
      title: "",
      leftFormat: statsDuration,
      box: sharedBox,
      nowMs: chartNow,
      tooltipRows: tooltipRows
    };
    var usageAxes = statsChartAxes(rows, usageOptions), latencyAxes = statsChartAxes(rows, latencyOptions);
    var chartPad = {left: Math.max(usageAxes.left, latencyAxes.left), right: Math.max(usageAxes.right, latencyAxes.right)};
    // Preserve a usable plot even with unusually wide numeric labels on phones.
    var padScale = Math.min(1, (Math.max(260, chartWidth) - 100) / (chartPad.left + chartPad.right));
    chartPad.left = Math.ceil(chartPad.left * padScale);
    chartPad.right = Math.ceil(chartPad.right * padScale);
    usageOptions.plotPad = latencyOptions.plotPad = chartPad;
    hideAllStatsChartHover();
    statsChartControllers = [];
    renderStatsChart("usageChart", rows, usageOptions);
    renderStatsChart("latencyChart", rows, latencyOptions);
    // 明细改为按模型排名，跟随同一时间范围与账号筛选；服务端已按用量排序并剔除无调用模型。
    var ranked = data.models || [];
    $("statsRange").textContent = data.start + " → " + data.end + " · " + ranked.length + " 个模型有调用";
    setHTMLIfChanged($("statsTable").querySelector("tbody"), ranked.length ? ranked.map(function (m, i) {
      return "<tr><td>" + (i + 1) + "</td><td class='mono'>" + esc(m.model) + "</td><td>" + esc(statsNumber(m.requests)) + "</td><td>" +
        esc(m.success + " / " + m.failed) + "</td><td>" + esc(m.tokenSamples ? statsNumber(m.tokens) : "—") + "</td><td>" +
        esc(m.tokenSamples ? statsNumber(m.inputTokens) + " / " + statsNumber(m.outputTokens) : "—") + "</td><td>" +
        esc(m.tokenSamples ? statsNumber(m.cachedTokens) : "—") + "</td><td>" +
        esc(m.creditSamples ? statsCredit(m.credits) : "—") + "</td><td>" + esc(statsDuration(m.avgLatencyMs)) + "</td><td>" +
        esc(statsDuration(tokenTime(m))) + "</td><td class='hint'>" + esc("Token " + m.tokenSamples + "/" + m.requests +
          " · 积分 " + m.creditSamples + "/" + m.requests + " · TTFT " + (currentTTFT ? m.ttftSamples : 0) + "/" + m.requests) + "</td></tr>";
    }).join("") : '<tr><td colspan="11" class="empty">所选范围没有模型调用记录</td></tr>');
    state.statsRenderKey = renderKey;
  }

  function loadStats(force) {
    if (!state.apiKey || state.statsPending || (!force && Date.now() - state.statsLastPoll < 10000)) return Promise.resolve();
    state.statsPending = true;
    var version = state.statsVersion, key = state.apiKey;
    var params = ["period=" + encodeURIComponent($("statsPeriod").value || "day")];
    var scope = state.statsAccount || "";
    if (scope.indexOf("site:") === 0) params.push("site=" + encodeURIComponent(scope.slice(5)));
    else if (scope) params.push("account=" + encodeURIComponent(scope));
    ["start", "end"].forEach(function (field) {
      var value = $(field === "start" ? "statsStart" : "statsEnd").value;
      if (value) params.push(field + "=" + encodeURIComponent(value));
    });
    // Background refresh keeps the current layout and any storage warning.
    if (!state.lastStats) $("statsInfo").textContent = "正在读取统计…";
    return api("/admin/api/stats?" + params.join("&")).then(function (data) {
      if (version !== state.statsVersion || key !== state.apiKey) return;
      renderStats(data);
      state.statsLastPoll = Date.now();
    }).catch(function (err) {
      if (err && err.status === 400 && state.statsAccount) {
        // 上次记住的账号已被删除时，退回全局查询而不是一直报错。
        state.statsAccount = "";
        rememberStatsFilter();
        state.statsVersion++;
        return;
      }
      if (version === state.statsVersion && key === state.apiKey) $("statsInfo").textContent = "统计读取失败，图表保留上次结果；请检查筛选日期或稍后重试。";
    }).then(function () {
      state.statsPending = false;
      if (version !== state.statsVersion && state.apiKey) return loadStats(true);
    });
  }

  function changeStatsFilter(resetDates) {
    state.statsVersion++;
    state.statsLastPoll = 0;
    if (resetDates) {
      $("statsStart").value = "";
      $("statsEnd").value = "";
      state.statsDatesTouched = false;
    }
    state.statsAccount = $("statsAccount").value || "";
    rememberStatsFilter();
    return loadStats(true);
  }

  var SITE_LABEL = { cn: "国内站", intl: "国际站" };

  function renderStatus(data) {
    state.lastStatus = data;
    var g = data.gateway || {};
    setHTMLIfChanged($("meta"),
      '<a class="chip chip-link" href="https://github.com/CangShui/workbuddy-gateway"' +
      ' target="_blank" rel="noreferrer" title="在新标签页打开项目地址">v' + esc(g.version) + '</a>' +
      '<span class="chip">' + esc(g.listen) + '</span>' +
      '<span class="chip">模型源 ' + esc(g.modelSource || "unavailable") + '</span>' +
      '<span class="chip">更新 ' + esc(fmtTime(data.updatedAt)) + '</span>');

    renderPoolHealth(data);
    if (state.lastAccountAlerts) renderAccountAlerts(state.lastAccountAlerts);

    setHTMLIfChanged($("gatewayInfo"), kvRow("版本", g.version) +
      kvRow("监听地址", g.listen) +
      kvRow("API 鉴权", g.apiKeyEnabled ? "已启用" : "未启用") +
      kvRow("管理台", g.webuiEnabled ? "已启用" : "未启用") +
      kvRow("模型来源", g.modelSource || "unavailable") +
      kvRow("快照更新", fmtTime(data.updatedAt)));

    renderAccounts(data.accounts || []);
    if (data.stale) showAlert("状态快照超过 15 秒未更新，当前展示的是上次数据，请检查网关状态。",
      false, "/admin/api/status");
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
  // 统计筛选（粒度/范围/起止日期）记在 Cookie 里，退出登录或重新打开页面都能恢复。
  // 只保存用户真正改过的日期：没动过就留空，继续用默认的滚动区间，避免把默认区间冻结住。
  var STATS_FILTER_COOKIE = "gateway_stats_filter";

  function readCookieRaw(name) {
    try {
      var cookies = String(document.cookie || "").split(";");
      for (var i = 0; i < cookies.length; i++) {
        var pair = cookies[i].trim();
        if (pair.indexOf(name + "=") === 0) return decodeURIComponent(pair.slice(name.length + 1));
      }
    } catch (_) { /* Cookie 被禁用时按默认值渲染，不影响登录与查询 */ }
    return "";
  }

  function writeCookieRaw(name, value) {
    try {
      document.cookie = name + "=" + encodeURIComponent(value) +
        "; Path=/ui; Max-Age=31536000; SameSite=Lax" +
        (window.location.protocol === "https:" ? "; Secure" : "");
      return readCookieRaw(name) === value;
    } catch (_) { return false; }
  }

  function readStatsFilterCookie() {
    var raw = readCookieRaw(STATS_FILTER_COOKIE);
    if (!raw) return null;
    try {
      var saved = JSON.parse(raw);
      if (!saved || typeof saved !== "object") return null;
      var periods = ["hour", "day", "week", "month"];
      return {
        period: periods.indexOf(saved.period) > -1 ? saved.period : "",
        account: typeof saved.account === "string" ? saved.account : "",
        start: typeof saved.start === "string" ? saved.start : "",
        end: typeof saved.end === "string" ? saved.end : ""
      };
    } catch (_) { return null; }
  }

  function writeStatsFilterCookie() {
    return writeCookieRaw(STATS_FILTER_COOKIE, JSON.stringify({
      period: $("statsPeriod").value || "day",
      account: state.statsAccount || "",
      start: state.statsDatesTouched ? ($("statsStart").value || "") : "",
      end: state.statsDatesTouched ? ($("statsEnd").value || "") : ""
    }));
  }

  function initStatsFilter() {
    var saved = readStatsFilterCookie();
    if (!saved) return;
    if (saved.period) $("statsPeriod").value = saved.period;
    state.statsAccount = saved.account;
    if (saved.start || saved.end) {
      $("statsStart").value = saved.start;
      $("statsEnd").value = saved.end;
      state.statsDatesTouched = true;
    }
  }

  function rememberStatsFilter() {
    var remembered = writeStatsFilterCookie();
    var hint = $("statsFilterStatus");
    if (hint) {
      hint.textContent = remembered ? "" : "统计筛选已应用，但浏览器没能保存 Cookie，刷新后可能记不住这次选择。";
      hint.classList.toggle("hidden", remembered);
    }
    return remembered;
  }

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

  // ---- 账号：网页新增（复用 CLI 登录流程）与删除 ---------------------------
  function clearAddAccountArtifacts() {
    $("addAccountQr").removeAttribute("src");
    $("addAccountLink").href = "#";
    $("addAccountLink").textContent = "";
    var copy = $("addAccountCopy");
    if (copy) {
      copy.disabled = true;
      copy.textContent = "复制链接";
    }
  }

  function fallbackCopyText(text, done) {
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.top = "-1000px";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (_) {
      ok = false;
    }
    document.body.removeChild(area);
    done(ok);
  }

  function copyAddAccountLink() {
    var link = $("addAccountLink");
    var text = (link.textContent || "").replace(/^\s+|\s+$/g, "");
    if (!text) return;
    var button = $("addAccountCopy");
    var done = function (ok) {
      button.textContent = ok ? "已复制" : "复制失败";
      if (!ok) {
        // 非安全上下文或浏览器拒绝剪贴板权限时，至少把链接选中，方便手动复制。
        try {
          var range = document.createRange();
          range.selectNodeContents(link);
          var selection = window.getSelection();
          selection.removeAllRanges();
          selection.addRange(range);
        } catch (_) { /* 选中失败不影响提示 */ }
      }
      window.setTimeout(function () { button.textContent = "复制链接"; }, 1500);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        navigator.clipboard.writeText(text).then(function () {
          done(true);
        }, function () {
          fallbackCopyText(text, done);
        });
        return;
      } catch (_) { /* 继续走 execCommand 回退 */ }
    }
    fallbackCopyText(text, done);
  }

  function addAccountRemainingSeconds() {
    if (!state.addAccountDeadline) return 0;
    return Math.max(0, Math.ceil((state.addAccountDeadline - Date.now()) / 1000));
  }

  function fmtAddAccountRemaining(seconds) {
    var m = Math.floor(seconds / 60);
    var s = seconds % 60;
    return m + ":" + (s < 10 ? "0" : "") + s;
  }

  function showAddAccountPending() {
    var left = addAccountRemainingSeconds();
    $("addAccountStatus").textContent = "请用手机扫码完成授权；成功后凭据会自动保存并加载。" +
      (left > 0 ? "剩余 " + fmtAddAccountRemaining(left) + "，超时后需重新获取。"
        : "登录链接即将失效，请重新获取。");
  }

  function showAddAccountExpired() {
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    clearAddAccountArtifacts();
    $("addAccountStatus").textContent = "登录链接已失效，请重新点击「获取登录接口」。";
  }

  function openAddAccount() {
    $("addAccountPanel").classList.remove("hidden");
    $("addAccountResult").classList.add("hidden");
    clearAddAccountArtifacts();
    $("addAccountStatus").textContent = "正在读取默认凭据名…";
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    return api("/admin/api/account-add").then(function (data) {
      $("addFile").value = data.defaultFile || "";
      $("addAccountStatus").textContent = "";
    }).catch(function () {
      $("addAccountStatus").textContent = "读取默认凭据名失败，可手动填写。";
    });
  }

  function closeAddAccount() {
    var id = state.addAccountId;
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    $("addAccountPanel").classList.add("hidden");
    $("addAccountResult").classList.add("hidden");
    clearAddAccountArtifacts();
    if (id) return api("/admin/api/account-cancel", {method: "POST", body: {id: id}}).catch(function () {});
    return Promise.resolve();
  }

  function startAddAccount() {
    var site = $("addSite").value === "intl" ? "intl" : "cn";
    var file = ($("addFile").value || "").replace(/^\s+|\s+$/g, "");
    if (!file) { $("addAccountStatus").textContent = "请先填写凭据名。"; return; }
    if (state.addAccountId) closeAddAccount();
    $("addStart").disabled = true;
    $("addAccountStatus").textContent = "正在获取登录接口…";
    return api("/admin/api/account-add", {method: "POST", body: {site: site, file: file}}).then(function (data) {
      state.addAccountId = data.id || "";
      state.addAccountPoll = 0;
      state.addAccountDeadline = data.remainingSeconds
        ? Date.now() + Number(data.remainingSeconds) * 1000
        : (data.expiresAt ? Number(data.expiresAt) * 1000 : 0);
      $("addAccountResult").classList.remove("hidden");
      $("addAccountQr").src = data.qrPng || "";
      $("addAccountLink").href = data.authUrl || "#";
      $("addAccountLink").textContent = data.authUrl || "";
      $("addAccountCopy").disabled = !data.authUrl;
      showAddAccountPending();
      pollAddAccount();
    }).catch(function (err) {
      $("addAccountStatus").textContent = "获取登录接口失败：" + ((err && err.detail) || "请稍后重试");
    }).then(function () { $("addStart").disabled = false; });
  }

  function pollAddAccount(force) {
    if (!state.addAccountId || !state.apiKey) return Promise.resolve();
    if (!force && Date.now() - state.addAccountPoll < 2000) return Promise.resolve();
    state.addAccountPoll = Date.now();
    var id = state.addAccountId;
    return api("/admin/api/account-add?id=" + encodeURIComponent(id), {quiet: true}).then(function (data) {
      if (state.addAccountId !== id) return;
      var status = data.status || "pending";
      if (status === "pending") {
        if (typeof data.remainingSeconds === "number") {
          state.addAccountDeadline = Date.now() + data.remainingSeconds * 1000;
        }
        showAddAccountPending();
        return;
      }
      state.addAccountId = "";
      state.addAccountDeadline = 0;
      if (status === "done") {
        $("addAccountStatus").textContent = "登录成功，凭据已保存为 " + (data.savedAs || "") + "，账号池已自动加载。";
        showAlert("账号已添加：" + (data.savedAs || ""), true);
        refreshActive();
      } else if (status === "expired") {
        showAddAccountExpired();
      } else if (status === "cancelled") {
        clearAddAccountArtifacts();
        $("addAccountStatus").textContent = "登录已取消。";
      } else {
        clearAddAccountArtifacts();
        $("addAccountStatus").textContent = "登录失败：" + (data.error || "请重试");
      }
    }).catch(function (err) {
      if (state.addAccountId !== id) return;
      if (err && err.status === 404) {
        showAddAccountExpired();
        return;
      }
      $("addAccountStatus").textContent = "查询登录进度失败，保留上次结果。";
    });
  }

  function accountRowActions(event) {
    var target = event && event.target;
    if (!target || typeof target.closest !== "function") return;
    var btn = target.closest("[data-account-delete]");
    if (!btn) return;
    var name = btn.getAttribute("data-account-delete");
    if (!name) return;
    if (!window.confirm("确定删除凭据 " + name + " ？\n\n" +
        "会从账号池移除该账号，并删除运行目录下的对应凭据文件。统计与历史日志保留，操作不可撤销。")) return;
    state.policyVersion++;
    api("/admin/api/account-delete", {method: "POST", body: {file: name}}).then(function () {
      showAlert("已删除凭据 " + name + "，账号池已同步移除。", true);
      return refreshActive();
    }).catch(function (err) {
      showAlert("删除失败：" + ((err && err.detail) || "请稍后重试"), false);
    });
  }

  function checkinCell(a) {
    var c = a.checkin || {}, now = Date.now();
    var today = new Date(now + 8 * 3600000).toISOString().slice(0, 10);
    var status = c.status || "pending", message = c.message || "";
    if (a.edition === "intl" || status === "unsupported") {
      return '<span class="badge" title="国际站或企业账号不参与个人签到">—</span>';
    }
    if (c.date !== today) {
      var hour = new Date(now + 8 * 3600000).getUTCHours();
      var minute = new Date(now + 8 * 3600000).getUTCMinutes();
      status = hour * 60 + minute >= 570 ? "missed" : "pending";
      message = status === "missed" ? "今日窗口已过，不补签" : "等待北京时间今天的随机计划";
    }
    var labels = {pending: "待签", running: "待签", success: "已签", failed: "失败", missed: "未签", unknown: "未知"};
    var styles = {success: "ok", failed: "danger", unknown: "warn"};
    var detail = "北京时间 " + today + " · " + message;
    if (c.date === today && c.nextAt) {
      detail += " · 计划 " + new Date(c.nextAt * 1000).toLocaleString("zh-CN", {timeZone: "Asia/Shanghai", hour12: false});
    }
    return '<span class="badge ' + (styles[status] || "") + '" title="' + esc(detail) + '">' +
      (labels[status] || "未知") + '</span>';
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
      updateTableBody(tb, '<tr><td colspan="16" class="empty">' +
        (onlyUsable ? "没有符合条件的可用账号，取消勾选可查看全部账号。" : "暂无账号数据（服务是否已写入状态快照？）") + '</td></tr>');
      return;
    }
    var html = list.map(function (a, i) {
      var st = STATE_TEXT[a.state] || a.state || "-";
      var cls = STATE_CLASS[a.state] || "";
      if (a.schedulingEnabled === false) {
        st = a.state === "active" ? "已停用" : "已停用 · " + st;
        cls = "warn";
      }
      var site = a.edition === "intl" ? "国际站" : "国内站";
      return '<tr data-row-key="' + esc(a.path) + '">' +
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
        "<td>" + checkinCell(a) + "</td>" +
        "<td>" + esc(count(a.modelCooldowns)) + "</td>" +
        "<td>" + esc(fmtTime(a.lastRefreshTime)) + "</td>" +
        "<td>" + esc(count(a.refreshFailCount)) + "</td>" +
        "<td><button class=\"ghost danger\" type=\"button\" data-account-delete=\"" +
        esc(a.path) + "\">删除</button></td>" +
        "</tr>";
    }).join("");
    updateTableBody(tb, html);
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
    var rateChanged = data.rateRevision !== undefined && state.rateRevision !== data.rateRevision;
    if (rateChanged) {
      state.rateRevision = data.rateRevision;
      state.rateDetails = null;
      state.rateDetailsAt = 0;
    }
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
      updateTableBody(tb, '<tr><td colspan="10" class="empty">' +
        (onlyUsable ? "没有符合条件的可用模型，取消勾选可查看全部模型。" : "暂无模型数据") + '</td></tr>');
      return;
    }
    var html = list.map(function (m) {
      return '<tr data-row-key="' + esc(m.id) + '">' +
        "<td>" + policySwitch("model", m.id, m.enabled !== false, m.policyReason) + "</td>" +
        "<td class='mono'>" + esc(m.id) + "</td>" +
        rateCell("cn", m.id, m.cnMultiplier) +
        rateCell("intl", m.id, m.intlMultiplier) +
        "<td>" + esc(count(m.availableAccounts)) + "</td>" +
        "<td>" + esc(count(m.requests)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgTtftMs, m.hasTtft && m.ttftMetric === "first_token_v2")) + "</td>" +
        "<td>" + esc(fmtMs(m.avgLatencyMs, m.hasLatency)) + "</td>" +
        "<td>" + esc(fmtTokensM(m.tokens)) + "</td>" +
        "<td>" + esc(m.lastStatus || "-") + "</td>" +
        "</tr>";
    }).join("");
    updateTableBody(tb, html);
    restorePolicyFocus(tb, focusTarget);
    restoreRateDetails(tb, rateChanged);
    if (data.stale) showAlert("模型统计快照过旧，当前展示的是上次数据。", false, "/admin/api/models");
  }

  // ---- 倍率来源：数值共用后端决策，原始日志只是可追溯证据 -----------------------
  function rateKey(site, model) {
    var id = String(model || "").trim().toLowerCase();
    if (site === "intl" && id.endsWith("-sg")) id = id.slice(0, -3);
    return site + "|" + id;
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
    var requestedRevision = state.rateRevision;
    state.rateDetailsPending = api("/admin/api/rate-detail").then(function (data) {
      if (requestedRevision === state.rateRevision) {
        state.rateDetails = data;
        state.rateDetailsAt = Date.now();
      }
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
    if (!info || (!info.hasSource && !info.hasLog)) {
      var missingHint = payload && payload.truncated ?
        "本次原始日志扫描未覆盖全部记录，暂未找到该模型的来源；这不代表模型没有被调用。" :
        (payload && payload.noLogHint || "尚无该模型的目录或有效实测来源，不能推断倍率。");
      if (payload && payload.logProblem) missingHint = payload.logProblem;
      return "<p class='rate-none'>暂未找到倍率来源</p><p class='hint'>" + esc(missingHint) + "</p>";
    }
    // 不再标注「状态：未知」：该结论是实测过的，写「未知」会让人误以为从未检测。
    var meta = "<div class='rate-meta'><span>来源：" + esc(info.source) + "</span>" +
      "<span>更新时间：" + esc(fmtTime(info.updatedAt)) + "</span></div>";
    var html = meta;
    if (info.value) html += "<p class='rate-current'>当前倍率：" + esc(info.value) + "</p>";
    if (info.explain) html += "<p class='rate-explain'>" + esc(info.explain) + "</p>";
    if (info.checkedAt) html += "<p class='hint'>目录最近成功读取：" + esc(fmtTime(info.checkedAt)) + "</p>";
    if (info.changedAt) html += "<p class='hint'>价格规则最近变化：" + esc(fmtTime(info.changedAt)) + "</p>";
    if (info.catalog) {
      html += "<p class='hint'>目录模型：" + esc(info.catalog.model || model) +
        "；原始 credits：" + esc(info.catalog.credits || "未提供") + "</p>";
      if (info.catalog.promoUntil) html += "<p class='hint'>促销截止：" + esc(info.catalog.promoUntil) + "</p>";
    }
    if (info.observation) {
      html += "<p class='hint'>最近实测：" + esc(fmtTime(info.observation.updatedAt)) +
        "；" + esc(info.observation.class === "free" ? "免费" : "收费") +
        (info.observation.credit != null ? "；usage.credit=" + esc(String(info.observation.credit)) : "；旧账本未保存具体积分") + "</p>";
      if (info.observation.freeUntil) html += "<p class='hint'>该免费样本关联的促销截止：" +
        esc(info.observation.freeUntil) + "；过期后不能继续当作当前免费依据。</p>";
    }
    if (info.probe) {
      html += "<p class='hint'>最近有效探测：" + esc(fmtTime(info.probe.updatedAt)) +
        "；" + esc(info.probe.verdict || "无结论") + "</p>";
      if (info.probe.lastError) html += "<p class='hint'>最近探测未成功：" + esc(info.probe.lastError) +
        "；失败不会更新上次有效结论的时间。</p>";
    }
    if (payload && payload.sourceProblem) html += "<p class='hint'>" + esc(payload.sourceProblem) + "</p>";
    if (payload && payload.logProblem) html += "<p class='hint'>" + esc(payload.logProblem) + "</p>";
    else if (info.hasSource && !info.hasLog) {
      html += "<p class='hint'>" + (payload && payload.truncated ?
        "当前来源已保存；原始日志未在本次扫描窗口内找到，不影响倍率结论。" :
        "当前来源已保存；原始普通日志暂未找到或已按保留策略清理，不影响倍率结论。") + "</p>";
    }
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
    if (!panel.innerHTML) panel.innerHTML = "<p class='hint'>读取倍率来源中…</p>";
    var requestedRevision = state.rateRevision;
    ensureRateDetails().then(function (payload) {
      if (!state.openRateKeys[rateKey(site, model)]) return;
      if (requestedRevision !== state.rateRevision) {
        fillRateDetail(button, panel);
        return;
      }
      var info = payload && payload.rates && payload.rates[rateKey(site, model)];
      var cell = button.closest("td");
      var valueElement = cell && cell.querySelector(".rate-value");
      if (info && valueElement && info.value) valueElement.textContent = info.value;
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

  // Replaced cells restore their details; unchanged expanded cells stay intact.
  function restoreRateDetails(table, force) {
    Array.prototype.forEach.call(table.querySelectorAll("button.rate-info"), function (button) {
      var key = rateKey(button.getAttribute("data-rate-site"), button.getAttribute("data-rate-model"));
      if (!state.openRateKeys[key]) return;
      var cell = button.closest("td");
      var panel = cell ? cell.querySelector(".rate-detail") : null;
      if (panel && (force || button.getAttribute("aria-expanded") !== "true" || panel.classList.contains("hidden"))) {
        fillRateDetail(button, panel);
      }
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
      if (data.error) { box.textContent = "\u8bfb\u53d6\u65e5\u5fd7\u5931\u8d25: " + data.error; return; }
      var lines = (data.lines || []).slice().reverse();
      box.textContent = lines.length ? lines.join("\n") : "\u8be5\u6587\u4ef6\u6682\u65e0\u65e5\u5fd7\u3002";
      box.scrollTop = 0;
      if (data.truncated) showAlert("\u65e5\u5fd7\u8fbe\u5230\u672c\u6b21\u8bfb\u53d6\u9884\u7b97\uff0c\u53ea\u5c55\u793a\u90e8\u5206\u884c\uff1b\u539f\u65e5\u5fd7\u6587\u4ef6\u672a\u4fee\u6539\u3002");
    }).catch(function () {
      if (!initial && file) return loadLogs(true);
    });
  }

  // ---- 渲染：配置 --------------------------------------------------------
  function jsonLiteralBoundary(text, end) {
    if (end >= text.length) return true;
    return !/[A-Za-z0-9_$]/.test(text.charAt(end));
  }

  function jsonNumberLength(text, start) {
    var i = start, length = text.length;
    if (text.charAt(i) === "-") i++;
    if (i >= length) return 0;
    if (text.charAt(i) === "0") {
      i++;
    } else if (text.charAt(i) >= "1" && text.charAt(i) <= "9") {
      while (i < length && text.charAt(i) >= "0" && text.charAt(i) <= "9") i++;
    } else {
      return 0;
    }
    if (text.charAt(i) === ".") {
      i++;
      if (text.charAt(i) < "0" || text.charAt(i) > "9") return 0;
      while (i < length && text.charAt(i) >= "0" && text.charAt(i) <= "9") i++;
    }
    if (text.charAt(i) === "e" || text.charAt(i) === "E") {
      i++;
      if (text.charAt(i) === "+" || text.charAt(i) === "-") i++;
      if (text.charAt(i) < "0" || text.charAt(i) > "9") return 0;
      while (i < length && text.charAt(i) >= "0" && text.charAt(i) <= "9") i++;
    }
    return i - start;
  }

  function jsonHighlightHTML(text) {
    var source = String(text === undefined || text === null ? "" : text);
    var out = [], plain = "", i = 0;
    function flush() {
      if (plain) {
        out.push(esc(plain));
        plain = "";
      }
    }
    function token(className, value) {
      flush();
      out.push('<span class="json-' + className + '">' + esc(value) + "</span>");
    }
    while (i < source.length) {
      var ch = source.charAt(i);
      if (ch === '"') {
        var start = i++;
        while (i < source.length) {
          if (source.charAt(i) === "\\") {
            i += 2;
            continue;
          }
          if (source.charAt(i) === '"') {
            i++;
            break;
          }
          i++;
        }
        var value = source.slice(start, i);
        var next = i;
        while (next < source.length && /\s/.test(source.charAt(next))) next++;
        token(source.charAt(next) === ":" ? "key" : "string", value);
        continue;
      }
      if (ch === "-" || (ch >= "0" && ch <= "9")) {
        var numberLength = jsonNumberLength(source, i);
        if (numberLength > 0) {
          token("number", source.slice(i, i + numberLength));
          i += numberLength;
          continue;
        }
      }
      if (source.slice(i, i + 4) === "true" && jsonLiteralBoundary(source, i + 4)) {
        token("boolean", "true");
        i += 4;
        continue;
      }
      if (source.slice(i, i + 5) === "false" && jsonLiteralBoundary(source, i + 5)) {
        token("boolean", "false");
        i += 5;
        continue;
      }
      if (source.slice(i, i + 4) === "null" && jsonLiteralBoundary(source, i + 4)) {
        token("null", "null");
        i += 4;
        continue;
      }
      if ("{}[]:,".indexOf(ch) >= 0) {
        token("punctuation", ch);
        i++;
        continue;
      }
      plain += ch;
      i++;
    }
    flush();
    return out.join("");
  }

  function syncConfigEditorHighlight() {
    var area = $("configEditor"), layer = $("configEditorHighlight");
    if (!area || !layer) return;
    layer.scrollTop = area.scrollTop;
    layer.scrollLeft = area.scrollLeft;
  }

  function renderConfigEditorHighlight() {
    var area = $("configEditor"), layer = $("configEditorHighlight");
    if (!area || !layer) return;
    layer.innerHTML = jsonHighlightHTML(area.value);
    syncConfigEditorHighlight();
  }

  function loadConfig() {
    var version = ++state.configPreviewVersion;
    return api("/admin/api/config").then(function (data) {
      if (version !== state.configPreviewVersion) return;
      var box = $("configBox");
      if (!data.exists) { box.textContent = "未找到 " + (data.path || "config.json"); return; }
      box.innerHTML = jsonHighlightHTML(data.content);
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
    $("configEditorHighlight").innerHTML = "";
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
      $("configEditor").scrollTop = 0;
      $("configEditor").scrollLeft = 0;
      renderConfigEditorHighlight();
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
      if (state.configMode === "edit") {
        var area = $("configEditor");
        area.focus();
        if (area.setSelectionRange) area.setSelectionRange(0, 0);
        area.scrollTop = 0;
        area.scrollLeft = 0;
        renderConfigEditorHighlight();
      }
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
    if (!window.confirm("确认将整份配置写入服务器 config.json？\n密钥、规则、提示词和监听端口都会立即热切换；已有连接与流式请求继续完成。日志保留规则可能触发不可撤销的删除。" +
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
      var message = (data.settings && data.settings.restartRequired ?
        "配置已保存，但监听端口尚未热切换，请检查服务器日志。" :
        "配置已保存，全部配置项已热生效，监听端口已同步。") +
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
      if (state.activeTab === "overview") {
        renderPoolHealth({stale: true});
      }
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("连接失败", "danger");
      }
    };
    var pending = Promise.resolve();
    if (state.activeTab === "overview" || state.activeTab === "accounts") {
      pending = api("/admin/api/status").then(function (data) {
        if (!state.togglePending && policyVersion === state.policyVersion) renderStatus(data);
      }).catch(onErr).then(function () {
        // 统计是附加信息：不并入 refreshActive 的完成链，避免统计变慢时拖住状态轮询。
        if (state.activeTab === "overview") {
          loadStats(false);
          loadAccountAlerts();
        }
        if (state.activeTab === "accounts") pollAddAccount();
      });
    } else if (state.activeTab === "models") {
      pending = api("/admin/api/models?range=5h").then(function (data) {
        if (!state.togglePending && policyVersion === state.policyVersion) renderModels(data);
      }).catch(onErr);
    } else if (state.activeTab === "redact") {
      pending = loadRedactRecords();
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
    if (name !== "overview") hideAllStatsChartHover();
    if (name !== "overview" && $("accountAlertTooltip")) $("accountAlertTooltip").hidden = true;
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
    else if (name === "redact") { state.redactDirty = false; loadRedact(); refreshActive(); }
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
    $("portSettingsStatus").textContent = (data.restartRequired ? "端口配置尚未切换，请检查服务器日志。 " :
      "配置端口与实际监听一致。 ") + (data.apiPortOverridden ? "API 端口被启动参数 -port 覆盖，配置值不会改变实际监听。 " : "") +
      "修改并保存后立即热切换监听；已有连接和流式请求继续完成，请用新端口重新打开控制台。";
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
        showAlert("端口已写入 config.json 并热切换；已有连接继续完成，请用新端口重新打开控制台。", true);
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

  // ---- 「脱敏」页：设置与脱敏记录（参照 magpie 观测 › 脱敏） --------------------
  var REDACT_KIND_LABELS = {API_KEY: "API Key", TOKEN: "Token", SECRET: "密钥字段", PASSWORD: "密码", PRIVATE_KEY: "私钥",
    EMAIL: "邮箱", PHONE: "手机号", ID_CARD: "身份证号", BANK_CARD: "银行卡号", TERM: "自定义词", FAILED: "脱敏失败"};
  var REDACT_SECRET_KINDS = {API_KEY: true, TOKEN: true, SECRET: true, PASSWORD: true, PRIVATE_KEY: true};
  var REDACT_PLACEHOLDER = /\{\{[A-Z][A-Z0-9_]*_[a-z2-7]{8}\}\}/g;

  function redactKindLabel(k) { return REDACT_KIND_LABELS[k] || k; }

  function redactKindChip(k, n) {
    return '<span class="redact-kind' + (REDACT_SECRET_KINDS[k] ? " secret" : "") + '">' + esc(redactKindLabel(k)) +
      (n > 1 ? " ×" + n : "") + "</span>";
  }

  function redactKindsText(kinds) {
    var parts = Object.keys(kinds || {}).sort().map(function (k) { return redactKindLabel(k) + " " + kinds[k]; });
    return parts.length ? "（" + parts.join("、") + "）" : "";
  }

  function redactHintText(h) {
    return (h.head ? h.head + "…" : "") + "（" + h.len + " 字符）";
  }

  function redactOptions(list, selected, allLabel) {
    if (selected && list.indexOf(selected) === -1) list = list.concat([selected]);
    return '<option value="">' + esc(allLabel) + "</option>" + list.map(function (v) {
      return '<option value="' + esc(v) + '"' + (v === selected ? " selected" : "") + ">" + esc(redactKindLabel(v)) + "</option>";
    }).join("");
  }

  function renderRedact(data) {
    state.redactSettings = data;
    var on = !!data.active;
    $("redactState").textContent = on ? "已开启" : "未开启";
    $("redactState").className = "badge" + (on ? " ok" : "");
    $("redactSecrets").checked = !!data.secrets;
    $("redactPersonal").checked = !!data.personal;
    if (!state.redactDirty) $("redactWords").value = (data.words || []).join("\n");
    $("redactStatus").textContent = on ? "已开启：后续模型请求发往上游前在本机脱敏，回复中的占位符自动还原。"
      : "未开启：请求原样发往上游。开启后才会产生脱敏记录。";
  }

  function loadRedact() {
    return api("/admin/api/redact", {quiet: true}).then(renderRedact).catch(function () {});
  }

  function redactQuery() {
    var q = "days=" + encodeURIComponent($("redactDays").value || "7") + "&limit=" + state.redactLimit;
    if ($("redactKindFilter").value) q += "&kind=" + encodeURIComponent($("redactKindFilter").value);
    if ($("redactModelFilter").value) q += "&model=" + encodeURIComponent($("redactModelFilter").value);
    return "/admin/api/redact-records?" + q;
  }

  function loadRedactRecords() {
    var version = ++state.redactVersion;
    return api(redactQuery(), {quiet: true}).then(function (data) {
      if (version === state.redactVersion) renderRedactRecords(data);
    }).catch(function () {});
  }

  function renderRedactRecords(data) {
    state.lastRedact = data;
    var s = data.summary || {}, kinds = s.kinds || {}, models = s.models || {};
    var kindNames = Object.keys(kinds).sort(function (a, b) { return kinds[b] - kinds[a]; });
    var modelNames = Object.keys(models).sort(function (a, b) { return models[b] - models[a]; });
    var topModel = modelNames.length ? modelNames[0] + " · " + models[modelNames[0]] + " 次" : "暂无";
    setHTMLIfChanged($("redactSummary"),
      statsCard("脱敏请求", statsNumber(s.requests || 0), "失败未发送 " + (s.failed || 0) + " · 换账号重试 " + (s.retried || 0), "neutral") +
      statsCard("替换处数", statsNumber(s.values || 0), s.requests ? "平均每请求 " + ((s.values || 0) / s.requests).toFixed(1) + " 处" : "上游没有看到任何原值", "token") +
      statsCard("回复还原", statsNumber(s.restored || 0), "回复中占位符换回原值的次数", "credit") +
      statsCard("涉及模型", statsNumber(modelNames.length), topModel, "first"));
    var range = {1: "今天", 7: "近 7 天", 30: "近 30 天"}[data.days] || ("近 " + data.days + " 天");
    $("redactRangeText").textContent = range;
    var max = kindNames.length ? kinds[kindNames[0]] : 0;
    setHTMLIfChanged($("redactKinds"), kindNames.length ? kindNames.map(function (k) {
      return '<div class="redact-bar"><span>' + redactKindChip(k, 1) + '</span><span class="redact-bar-track"><i data-pct="' +
        Math.max(2, Math.round(kinds[k] * 100 / max)) + '"></i></span><span class="redact-bar-count">' + statsNumber(kinds[k]) + "</span></div>";
    }).join("") : '<p class="stats-empty">' + esc(range) + "没有脱敏记录</p>");
    // CSP 禁止内联 style 属性；与健康度进度条一样由脚本设置宽度。
    Array.prototype.forEach.call($("redactKinds").querySelectorAll("[data-pct]"), function (bar) {
      bar.style.width = bar.getAttribute("data-pct") + "%";
    });
    setHTMLIfChanged($("redactKindFilter"), redactOptions(kindNames.concat(s.failed ? ["FAILED"] : []), $("redactKindFilter").value, "全部类型"));
    setHTMLIfChanged($("redactModelFilter"), redactOptions(modelNames, $("redactModelFilter").value, "全部模型"));
    var top = s.topValues || [];
    updateTableBody($("redactTopTable").querySelector("tbody"), top.length ? top.map(function (v) {
      return '<tr data-row-key="' + esc(v.placeholder) + '"><td>' + redactKindChip(v.kind, 1) + "</td><td>" + esc(redactHintText(v)) +
        '</td><td class="redact-ph">' + esc(v.placeholder) + "</td><td>" + statsNumber(v.count) + "</td><td>" + statsNumber(v.requests) + "</td></tr>";
    }).join("") : '<tr><td colspan="5" class="empty">暂无记录</td></tr>');
    var records = data.records || [];
    updateTableBody($("redactRecordsTable").querySelector("tbody"), records.length ? records.map(function (r) {
      var counts = {};
      (r.hits || []).forEach(function (h) { counts[h.kind] = (counts[h.kind] || 0) + h.count; });
      var chips = Object.keys(counts).sort().map(function (k) { return redactKindChip(k, counts[k]); }).join("");
      return '<tr data-row-key="' + esc(r.id) + '"' + (r.id === state.redactSelected ? ' class="selected"' : "") + ">" +
        "<td>" + esc(fmtTime(Math.floor(r.at / 1000))) + '</td><td class="mono">' + esc(r.endpoint || "-") + "</td><td>" + esc(r.model || "-") +
        '</td><td class="mono">' + esc(r.account || "-") + "</td><td>" + statsNumber(r.values) + '</td><td class="kinds">' + (chips || "-") +
        "</td><td>" + statsNumber(r.restored) + "</td><td>" + statsNumber(r.attempts) + "</td><td>" +
        (r.failed ? '<span class="badge danger">失败未发送</span>' : '<span class="badge ok">已脱敏发送</span>') +
        '</td><td><button class="ghost" type="button" data-redact-id="' + esc(r.id) + '">详情</button></td></tr>';
    }).join("") : '<tr><td colspan="10" class="empty">' + (state.redactSettings && !state.redactSettings.active ?
      "脱敏未开启：在下方“脱敏设置”中开启后，脱敏过的请求会出现在这里" : esc(range) + "没有符合条件的脱敏记录") + "</td></tr>");
    $("redactRecordsInfo").textContent = records.length ? "显示 " + records.length + " 条，最新在前；点击一行查看详情" : "";
    $("redactMore").classList.toggle("hidden", !data.more);
  }

  function openRedactDetail(id) {
    state.redactSelected = id;
    state.redactRevealed = null;
    state.redactDetail = null;
    $("redactDetailCard").classList.remove("hidden");
    $("redactDetailTitle").textContent = "记录详情 · 加载中…";
    if (state.lastRedact) renderRedactRecords(state.lastRedact);
    var version = ++state.redactDetailVersion;
    return api("/admin/api/redact-record?id=" + encodeURIComponent(id)).then(function (data) {
      if (version !== state.redactDetailVersion) return;
      state.redactDetail = data;
      renderRedactDetail();
      if ($("redactDetailCard").scrollIntoView) $("redactDetailCard").scrollIntoView({block: "nearest"});
    }).catch(function () {
      if (version === state.redactDetailVersion) $("redactDetailTitle").textContent = "记录详情 · 读取失败";
    });
  }

  function redactMarkText(text, values) {
    return esc(text).replace(REDACT_PLACEHOLDER, function (p) {
      if (values && Object.prototype.hasOwnProperty.call(values, p)) {
        return '<mark class="value" title="' + esc(p) + '">' + esc(values[p]) + "</mark>";
      }
      return "<mark>" + p + "</mark>";
    });
  }

  function renderRedactDetail() {
    var d = state.redactDetail;
    if (!d) return;
    var values = state.redactRevealed;
    $("redactDetailTitle").textContent = "记录详情 · " + fmtTime(Math.floor(d.at / 1000));
    $("redactDetailMeta").textContent = "接口 " + (d.endpoint || "-") + " · 模型 " + (d.model || "-") + " · 账号 " + (d.account || "-") +
      " · 替换 " + d.values + " 处 · 回复还原 " + d.restored + " 次 · 尝试 " + d.attempts + " 次" +
      (d.traceId ? " · traceId " + d.traceId : "") + (d.requestId ? " · requestId " + d.requestId : "");
    var reveal = $("redactReveal");
    reveal.disabled = !d.valuesInMemory;
    reveal.textContent = values ? "隐藏原值" : (d.valuesInMemory ? "显示原值" : "原值已不在内存");
    var hits = d.hits || [];
    updateTableBody($("redactHitsTable").querySelector("tbody"), hits.length ? hits.map(function (h) {
      var original = values ? (Object.prototype.hasOwnProperty.call(values, h.placeholder) ? '<span class="mono">' + esc(values[h.placeholder]) + "</span>" : "已不在内存")
        : (d.valuesInMemory ? "已隐藏" : "已不在内存");
      return '<tr data-row-key="' + esc(h.placeholder) + '"><td>' + redactKindChip(h.kind, 1) + '</td><td class="redact-ph">' + esc(h.placeholder) +
        "</td><td>" + esc(redactHintText(h)) + "</td><td>" + original + "</td><td>" + statsNumber(h.count) + "</td></tr>";
    }).join("") : '<tr><td colspan="5" class="empty">' + (d.failed ? "请求体无法解析，脱敏失败，未发送上游" : "无") + "</td></tr>");
    var segments = d.segments || [];
    $("redactSegments").innerHTML = segments.length ? segments.map(function (seg) {
      return '<div class="redact-segment"><div class="redact-segment-where">' + esc(seg.where) + "</div><pre>" +
        redactMarkText(seg.text, values) + "</pre></div>";
    }).join("") : '<p class="redact-gone">' + (d.failed ? "脱敏失败的请求不会发往上游。" :
      "这条请求的文字只保存在网关内存中，网关重启后（或超出最近 300 条）已不可查看；记录仍保留每个值的类型、长度和开头字符。") + "</p>";
  }

  function toggleRedactReveal() {
    var d = state.redactDetail;
    if (!d) return Promise.resolve();
    if (state.redactRevealed) {
      state.redactRevealed = null;
      renderRedactDetail();
      return Promise.resolve();
    }
    $("redactReveal").disabled = true;
    return api("/admin/api/redact-reveal", {method: "POST", body: {id: d.id}}).then(function (data) {
      if (state.redactDetail === d) {
        state.redactRevealed = data.values || {};
        renderRedactDetail();
      }
    }).catch(function () {}).then(function () { if (state.redactDetail === d) $("redactReveal").disabled = !d.valuesInMemory; });
  }

  function closeRedactDetail() {
    state.redactSelected = "";
    state.redactDetail = null;
    state.redactRevealed = null;
    state.redactDetailVersion++;
    $("redactDetailCard").classList.add("hidden");
    $("redactSegments").innerHTML = "";
    if (state.lastRedact) renderRedactRecords(state.lastRedact);
  }

  function clearRedactRecords() {
    if (!window.confirm("清空全部脱敏记录？\n\n会删除内存中的记录和 logs/redact-*.jsonl，不可撤销。脱敏设置不变，之后的请求会重新记录。")) {
      return Promise.resolve();
    }
    $("redactClear").disabled = true;
    return api("/admin/api/redact-records-clear", {method: "POST", body: {}}).then(function () {
      closeRedactDetail();
      showAlert("脱敏记录已清空；脱敏设置未改变。", true);
      return loadRedactRecords();
    }).catch(function () {}).then(function () { $("redactClear").disabled = false; });
  }

  function setRedactBusy(busy) {
    ["redactSecrets", "redactPersonal", "redactWords", "redactSave", "redactTryBtn"].forEach(function (id) { $(id).disabled = busy; });
  }

  function saveRedact() {
    var words = $("redactWords").value.split(/\r?\n/).map(function (w) { return w.trim(); })
      .filter(function (w) { return w !== ""; });
    var short = words.filter(function (w) { return Array.from(w).length < 2; });
    if (short.length) {
      $("redactStatus").textContent = "自定义词每项至少 2 个字符：" + short.join("、");
      return Promise.resolve();
    }
    setRedactBusy(true);
    return api("/admin/api/redact", {method: "POST", body: {secrets: $("redactSecrets").checked,
      personal: $("redactPersonal").checked, words: words}}).then(function (data) {
      state.redactDirty = false;
      renderRedact(data);
      showAlert(data.active ? "请求脱敏已开启：已写入 config.json，后续模型请求立即生效。" :
        "请求脱敏已关闭：已写入 config.json，后续请求原样发往上游。", true);
      loadRedactRecords();
    }).catch(function (err) {
      if (err && err.detail) $("redactStatus").textContent = "保存失败：" + err.detail + "；输入已保留。";
    }).then(function () { setRedactBusy(false); });
  }

  function previewRedact() {
    var text = $("redactTryInput").value;
    if (!text) { $("redactTryOutput").textContent = ""; return Promise.resolve(); }
    setRedactBusy(true);
    return api("/admin/api/redact-preview", {method: "POST", body: {text: text}}).then(function (data) {
      var counts = {};
      (data.hits || []).forEach(function (h) { counts[h.kind] = (counts[h.kind] || 0) + 1; });
      var note = data.hits.length ? "命中 " + data.hits.length + " 处" + redactKindsText(counts) : "未发现需要脱敏的内容";
      if (data.off) note += "；当前未开启，按“密钥与凭据 + 个人信息”预览";
      $("redactTryOutput").textContent = data.masked + "\n\n— " + note;
    }).catch(function () {}).then(function () { setRedactBusy(false); });
  }

  function resetRedactPage() {
    state.redactDirty = false;
    state.redactVersion++;
    state.redactDetailVersion++;
    state.redactSelected = "";
    state.redactDetail = null;
    state.redactRevealed = null;
    state.lastRedact = null;
    state.redactSettings = null;
    state.redactLimit = 50;
    $("redactDetailCard").classList.add("hidden");
    ["redactWords", "redactTryInput"].forEach(function (id) { $(id).value = ""; });
    ["redactTryOutput", "redactStatus", "redactRecordsInfo", "redactDetailMeta"].forEach(function (id) { $(id).textContent = ""; });
    ["redactSummary", "redactKinds", "redactSegments"].forEach(function (id) { $(id).innerHTML = ""; $(id)._sourceHTML = ""; });
    ["redactRecordsTable", "redactTopTable", "redactHitsTable"].forEach(function (id) {
      var body = $(id).querySelector("tbody");
      body.innerHTML = "";
      body._tableSourceHTML = "";
    });
    $("redactState").textContent = "未开启";
    $("redactState").className = "badge";
  }

  function initRedactPage() {
    ["redactDays", "redactKindFilter", "redactModelFilter"].forEach(function (id) {
      $(id).addEventListener("change", function () { state.redactLimit = 50; loadRedactRecords(); });
    });
    $("redactMore").addEventListener("click", function () {
      state.redactLimit = Math.min(200, state.redactLimit + 50);
      loadRedactRecords();
    });
    $("redactRecordsTable").addEventListener("click", function (e) {
      var row = e.target && e.target.closest ? e.target.closest("tr[data-row-key]") : null;
      if (row) openRedactDetail(row.getAttribute("data-row-key"));
    });
    $("redactReveal").addEventListener("click", toggleRedactReveal);
    $("redactDetailClose").addEventListener("click", closeRedactDetail);
    $("redactClear").addEventListener("click", clearRedactRecords);
    $("redactSave").addEventListener("click", saveRedact);
    $("redactTryBtn").addEventListener("click", previewRedact);
    $("redactWords").addEventListener("input", function () { state.redactDirty = true; });
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
    initStatsFilter();
    ["statsStart", "statsEnd"].forEach(function (id) {
      $(id).addEventListener("change", function () { state.statsDatesTouched = true; });
    });
    $("apiKey").value = state.apiKey;
    initTheme();
    initRedactPage();
    $("saveKey").addEventListener("click", saveKey);
    $("statsApply").addEventListener("click", function () { changeStatsFilter(false); });
    $("statsPeriod").addEventListener("change", function () { changeStatsFilter(true); });
    $("statsAccount").addEventListener("change", function () { changeStatsFilter(false); });
    // 图表 viewBox 跟随容器宽度，窗口尺寸变化后重绘一次（用 rAF 合并连续事件）。
    var statsResizePending = false;
    window.addEventListener("resize", function () {
      if (!state.lastStats || statsResizePending) return;
      statsResizePending = true;
      (window.requestAnimationFrame || function (fn) { fn(); })(function () {
        statsResizePending = false;
        if (state.lastStats) renderStats(state.lastStats);
      });
    });
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
    bindAccountAlertTooltip();
    $("editConfig").addEventListener("click", showConfigWarning);
    $("acknowledgeConfigEdit").addEventListener("click", loadConfigEditor);
    $("cancelConfigWarning").addEventListener("click", exitConfigEdit);
    $("cancelConfigEdit").addEventListener("click", exitConfigEdit);
    $("reloadConfigEditor").addEventListener("click", loadConfigEditor);
    $("saveConfig").addEventListener("click", saveConfigEditor);
    $("configEditor").addEventListener("input", function () {
      renderConfigEditorHighlight();
      $("configEditorStatus").textContent = configDirty() ? "有未保存的修改。离开配置页会提示确认；不会自动保存。" : "内容未修改。";
    });
    $("configEditor").addEventListener("scroll", syncConfigEditorHighlight);
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
    $("accountsTable").addEventListener("click", accountRowActions);
    $("addAccountBtn").addEventListener("click", openAddAccount);
    $("addStart").addEventListener("click", startAddAccount);
    $("addCancel").addEventListener("click", closeAddAccount);
    $("addAccountCopy").addEventListener("click", copyAddAccountLink);
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
