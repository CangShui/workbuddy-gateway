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
    openRawLogs: {},
    statsPending: false,
    statsVersion: 0,
    statsLastPoll: 0,
    lastStats: null,
    accountAlertsPending: false,
    accountAlertsLastPoll: 0,
    accountAlertsGeneration: 0,
    lastAccountAlerts: null
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
    state.statsVersion++;
    state.statsLastPoll = 0;
    state.lastStats = null;
    ["usageSummary", "usageChart", "latencyChart", "statsInfo", "statsRange"].forEach(function (id) {
      $(id).textContent = "";
    });
    if ($("latencyLegend")) $("latencyLegend").textContent = "";
    $("statsAccount").innerHTML = '<option value="">全局 · 全部账号</option>';
    $("statsTable").querySelector("tbody").innerHTML = "";
    $("statsStorage").textContent = "等待统计";
    state.accountAlertsGeneration++;
    state.accountAlertsLastPoll = 0;
    state.lastAccountAlerts = null;
    $("poolHealth").textContent = "—";
    $("poolHealth").className = "health-value";
    $("poolHealthBar").style.width = "0%";
    $("poolHealthStatus").textContent = "等待账号状态";
    $("accountAlertsInfo").textContent = "登录后读取账号异常日志。";
    $("accountAlerts").innerHTML = '<p class="stats-empty">等待日志</p>';
    ["meta", "gatewayInfo", "logBox", "configBox", "tasksInfo"].forEach(function (id) {
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
    var notes = ["来源：普通日志 · 只列当前异常，最新告警在上；等级仅区分颜色和标签"];
    if (data.stale || (snapshot && snapshot.stale)) notes.push("快照过旧，以下为上次确认的异常");
    if (data.missingLogCount) notes.push("部分异常暂未找到对应日志，可能等待下一次扫描");
    if (data.scanTruncated || data.listTruncated) notes.push("已达到读取上限，仅显示扫描范围内记录");
    $("accountAlertsInfo").textContent = notes.join("；") + "。";
    $("accountAlerts").innerHTML = alerts.length ? alerts.map(function (a) {
      var level = labels[a.severity] ? a.severity : "notice";
      return '<article class="account-alert alert-' + level + '"><div class="account-alert-head">' +
        '<span class="alert-priority">' + esc(labels[level]) + '</span><strong>' + esc(a.label) +
        '</strong><time>' + esc(fmtTime(a.at)) + '</time></div><div class="account-alert-file">' + esc(a.account) +
        '</div><p>' + esc(a.message) + '</p><span class="account-alert-source">日志：' + esc(a.logFile) + '</span></article>';
    }).join("") : '<p class="stats-empty">' + (data.missingLogCount ? "当前存在异常，但扫描范围内暂未找到对应日志" :
      data.stale ? "暂无日志记录，账号状态待确认" : "未发现当前账号异常日志") + '</p>';
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

  // 原生 SVG，不访问 CDN。空时间桶及未知耗时断开折线，不伪造连续的零值。
  // box 决定 viewBox 尺寸。让它贴近容器实际像素（比例≈1:1），
  // 字号才不会随缩放变小；窄屏用更矮的目标高度。
  // 原生 SVG，不加载 CDN。折线在时间桶缺样本处断开，不补零；柱形画在下层，折线覆盖在上层。
  // opts = {lines, bars, title, leftFormat, rightFormat, box}；给了 rightFormat 时柱形改用右轴。
  function statsComboChart(buckets, opts) {
    opts = opts || {};
    var lines = opts.lines || [], bars = opts.bars || [];
    var title = opts.title || "";
    var leftFormat = opts.leftFormat || function (v) { return String(v); };
    var rightFormat = opts.rightFormat || null;
    var box = opts.box || {w: 900, h: 260};
    var pick = function (s, b, i) {
      var v = s.value(b, i);
      return (v === null || v === undefined || !isFinite(v)) ? null : Number(v);
    };
    var gather = function (list) {
      var out = [];
      list.forEach(function (s) { buckets.forEach(function (b, i) {
        var v = pick(s, b, i);
        if (v !== null) out.push(v);
      }); });
      return out;
    };
    var lineValues = gather(lines), barValues = gather(bars);
    if (!lineValues.length && !barValues.length) return '<p class="stats-empty">该时间范围暂无可用样本</p>';
    var dual = !!rightFormat && barValues.length > 0;
    var scaleMax = function (values) {
      var m = values.length ? Math.max.apply(null, values) : 0;
      if (m <= 0) m = 1;
      return m * 1.12;
    };
    var leftMax = scaleMax(dual ? lineValues : lineValues.concat(barValues));
    var rightMax = scaleMax(dual ? barValues : []);
    var width = Math.max(260, Math.round(box.w)), height = Math.max(190, Math.round(box.h));
    var top = 18, bottom = 40;
    var leftTicks = [0, 1, 2, 3, 4].map(function (n) { return leftFormat(leftMax * (1 - n / 4)); });
    var rightTicks = dual ? [0, 1, 2, 3, 4].map(function (n) { return rightFormat(rightMax * (1 - n / 4)); }) : [];
    var left = Math.min(130, Math.max(52, 14 + Math.max.apply(null, leftTicks.map(function (t) { return t.length; })) * 7));
    var right = dual ? Math.min(130, Math.max(44, 14 + Math.max.apply(null, rightTicks.map(function (t) { return t.length; })) * 7)) : 16;
    var plotW = width - left - right, plotH = height - top - bottom;
    var x = function (i) { return buckets.length < 2 ? left + plotW / 2 : left + i * plotW / (buckets.length - 1); };
    var yLeft = function (v) { return top + plotH * (1 - v / leftMax); };
    var yRight = function (v) { return top + plotH * (1 - v / rightMax); };
    var out = '<svg viewBox="0 0 ' + width + ' ' + height + '" role="img" aria-label="' + esc(title) +
      '"><title>' + esc(title) + '</title>';
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
            '" y="' + yy.toFixed(2) + '" width="' + Math.max(1, barW - 1).toFixed(2) + '" height="' + h.toFixed(2) + '">' +
            '<title>' + esc(b.label + " · " + s.label + " " + (dual ? rightFormat : leftFormat)(value)) + '</title></rect>';
        });
      });
    }
    lines.forEach(function (s) {
      var points = [], paths = [];
      buckets.forEach(function (b, i) {
        var value = pick(s, b, i);
        if (value === null) {
          if (points.length) paths.push(points.join(" "));
          points = [];
          return;
        }
        var xx = x(i), yy = yLeft(value);
        points.push((points.length ? "L" : "M") + xx.toFixed(2) + "," + yy.toFixed(2));
        out += '<circle class="series-' + s.color + '" cx="' + xx + '" cy="' + yy + '" r="' + (buckets.length > 80 ? 1.8 : 3.2) +
          '"><title>' + esc(b.label + " · " + s.label + " " + leftFormat(value)) + '</title></circle>';
      });
      if (points.length) paths.push(points.join(" "));
      paths.forEach(function (p) { out += '<path class="chart-line series-' + s.color + '" d="' + p + '"/>'; });
    });
    return out + "</svg>";
  }

  function statsLineChart(buckets, series, title, format, box) {
    return statsComboChart(buckets, {lines: series, bars: [], title: title, leftFormat: format, box: box});
  }

  function renderStats(data) {
    state.lastStats = data;
    var s = data.summary || {}, rows = data.buckets || [], store = data.storage || {};
    var currentTTFT = data.ttftMetric === "first_token_v2";
    var tokenTime = function (row) { return currentTTFT ? row.avgTtftMs : null; };
    $("statsStorage").textContent = store.mode === "file" ? "stats.json · 已落盘" :
      (store.mode === "memory" ? "仅内存 · 未落盘" : "等待首次落盘");
    $("statsStorage").className = "badge " + (store.mode === "memory" ? "warn" : "");
    var started = data.startedAt ? new Date(data.startedAt).toLocaleString("zh-CN", {hour12: false}) : "尚未采集";
    $("statsInfo").textContent = "自 " + started + " 起采集；UTC+8，周从周一开始。按实际上游账号调用统计，失败重试分开记。 " +
      (store.message || "缺失的 usage 不当作零消耗；缓存读取包含在输入 Token 中，不重复相加。") +
      " 首Token按有效内容增量计时，仅完整步骤参与平均；旧首响应体样本不混入（排除 " + (s.legacyTtftSamples || 0) + " 条）。不等同于 dsh 当前会话范围。";
    $("statsStart").value = data.start;
    $("statsEnd").value = data.end;
    var choice = data.account || "";
    $("statsAccount").innerHTML = '<option value="">全局 · 全部账号</option>' + (data.accounts || []).map(function (a) {
      return '<option value="' + esc(a.id) + '"' + (a.id === choice ? " selected" : "") + '>' +
        esc(a.file + " · " + (a.edition === "intl" ? "国际" : "国内") + " · " + a.id.slice(0, 6)) + "</option>";
    }).join("");
    $("statsAccount").value = choice;
    var coverage = (s.tokenSamples || 0) + "/" + (s.requests || 0) + " 请求有 Token";
    $("usageSummary").innerHTML =
      statsCard("总消耗 Token", s.tokenSamples ? statsNumber(s.tokens) : "—", "输入 " + statsNumber(s.inputTokens) +
        " · 输出 " + statsNumber(s.outputTokens) + " · 缓存读取 " + statsNumber(s.cachedTokens), "token") +
      statsCard("总消耗积分", s.creditSamples ? statsCredit(s.credits) : "—", (s.creditSamples || 0) + "/" + (s.requests || 0) + " 请求有积分 · 仅计 usage.credit", "credit") +
      statsCard("账号调用次数", statsNumber(s.requests), "完整 " + statsNumber(s.success) + " · 失败 / 不完整 " + statsNumber(s.failed), "neutral") +
      statsCard("平均总耗时", statsDuration(s.avgLatencyMs), coverage, "token") +
      statsCard("平均首Token (TTFT)", statsDuration(tokenTime(s)), (currentTTFT ? (s.ttftSamples || 0) : 0) + "/" + (s.requests || 0) + " 请求有新口径有效样本", "first");
    var chartBox = function (id) {
      var el = $(id), w = el && el.clientWidth ? el.clientWidth : 900;
      return {w: w, h: w < 520 ? 230 : 280};
    };
    $("usageChart").innerHTML = statsComboChart(rows, {
      lines: [{label: "总 Token", color: "token", value: function (b) { return b.tokenSamples ? b.tokens : null; }}],
      bars: [{label: "积分", color: "credit", value: function (b) { return b.creditSamples ? b.credits : null; }}],
      title: "总 Token（折线，左轴）与积分（柱形，右轴）消耗趋势",
      leftFormat: function (v) { return v >= 1000000 ? (v / 1000000).toFixed(2) + "M" : statsNumber(v); },
      rightFormat: statsCredit,
      box: chartBox("usageChart")
    });
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
    $("latencyLegend").innerHTML = latencyLines.map(function (s) {
      return '<i class="series-dot ' + s.color + '"></i><span>' + esc(s.label) + "</span>";
    }).join("");
    $("latencyChart").innerHTML = statsComboChart(rows, {
      lines: latencyLines, bars: latencyBars,
      title: "各模型请求耗时：柱形=首Token，折线=总耗时。按当前时间和账号分别计算",
      leftFormat: statsDuration,
      box: chartBox("latencyChart")
    });
    // 明细改为按模型排名，跟随同一时间范围与账号筛选；服务端已按用量排序并剔除无调用模型。
    var ranked = data.models || [];
    $("statsRange").textContent = data.start + " → " + data.end + " · " + ranked.length + " 个模型有调用";
    $("statsTable").querySelector("tbody").innerHTML = ranked.length ? ranked.map(function (m, i) {
      return "<tr><td>" + (i + 1) + "</td><td class='mono'>" + esc(m.model) + "</td><td>" + esc(statsNumber(m.requests)) + "</td><td>" +
        esc(m.success + " / " + m.failed) + "</td><td>" + esc(m.tokenSamples ? statsNumber(m.tokens) : "—") + "</td><td>" +
        esc(m.tokenSamples ? statsNumber(m.inputTokens) + " / " + statsNumber(m.outputTokens) : "—") + "</td><td>" +
        esc(m.tokenSamples ? statsNumber(m.cachedTokens) : "—") + "</td><td>" +
        esc(m.creditSamples ? statsCredit(m.credits) : "—") + "</td><td>" + esc(statsDuration(m.avgLatencyMs)) + "</td><td>" +
        esc(statsDuration(tokenTime(m))) + "</td><td class='hint'>" + esc("Token " + m.tokenSamples + "/" + m.requests +
          " · 积分 " + m.creditSamples + "/" + m.requests + " · TTFT " + (currentTTFT ? m.ttftSamples : 0) + "/" + m.requests) + "</td></tr>";
    }).join("") : '<tr><td colspan="11" class="empty">所选范围没有模型调用记录</td></tr>';
  }

  function loadStats(force) {
    if (!state.apiKey || state.statsPending || (!force && Date.now() - state.statsLastPoll < 10000)) return Promise.resolve();
    state.statsPending = true;
    var version = state.statsVersion, key = state.apiKey;
    var params = ["period=" + encodeURIComponent($("statsPeriod").value || "day")];
    ["account", "start", "end"].forEach(function (field) {
      var value = $(field === "account" ? "statsAccount" : (field === "start" ? "statsStart" : "statsEnd")).value;
      if (value) params.push(field + "=" + encodeURIComponent(value));
    });
    $("statsInfo").textContent = "正在读取统计；图表与表格保持上次数据…";
    return api("/admin/api/stats?" + params.join("&")).then(function (data) {
      if (version !== state.statsVersion || key !== state.apiKey) return;
      renderStats(data);
      state.statsLastPoll = Date.now();
    }).catch(function () {
      if (version === state.statsVersion && key === state.apiKey) $("statsInfo").textContent = "统计读取失败，图表保留上次结果；请检查筛选日期或稍后重试。";
    }).then(function () {
      state.statsPending = false;
      if (version !== state.statsVersion && state.apiKey) return loadStats(true);
    });
  }

  function changeStatsFilter(resetDates) {
    state.statsVersion++;
    state.statsLastPoll = 0;
    if (resetDates) { $("statsStart").value = ""; $("statsEnd").value = ""; }
    return loadStats(true);
  }

  var SITE_LABEL = { cn: "国内站", intl: "国际站" };

  function renderStatus(data) {
    state.lastStatus = data;
    var g = data.gateway || {};
    $("meta").innerHTML =
      '<span class="chip">v' + esc(g.version) + '</span>' +
      '<span class="chip">' + esc(g.listen) + '</span>' +
      '<span class="chip">模型源 ' + esc(g.modelSource || "unavailable") + '</span>' +
      '<span class="chip">更新 ' + esc(fmtTime(data.updatedAt)) + '</span>';

    renderPoolHealth(data);
    if (state.lastAccountAlerts) renderAccountAlerts(state.lastAccountAlerts);

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
        "<td>" + esc(fmtMs(m.avgTtftMs, m.hasTtft && m.ttftMetric === "first_token_v2")) + "</td>" +
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
      });
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
