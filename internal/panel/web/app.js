/* xrayclient-reversepanel 管理网页（Vue 3，无构建步骤） */
(function () {
  "use strict";
  const { createApp, reactive, ref, computed, watch, nextTick } = Vue;

  // ---------------- 工具 ----------------
  const FPS = ["chrome", "firefox", "safari", "edge", "ios", "android", "360", "qq", "random", "randomized"];
  const NETS = { "tcp": "TCP", "udp": "UDP", "tcp,udp": "TCP+UDP" };
  const STATE_TEXT = { online: "在线", partial: "部分在线", connecting: "连接中", offline: "离线", stopped: "未运行", disabled: "已停用", pending: "待应用" };
  const STATE_BADGE = { online: "text-bg-success", partial: "text-bg-warning", connecting: "text-bg-warning", offline: "text-bg-danger", stopped: "text-bg-secondary", disabled: "text-bg-secondary", pending: "text-bg-info" };
  const BUSY_BPS = 64; // 高于这个速率（字节/秒）视为“传输中”，过滤掉心跳

  const clone = (o) => JSON.parse(JSON.stringify(o));
  const fmtBytes = (n) => { if (!n) return "0 B"; const u = ["B", "KB", "MB", "GB", "TB"]; let i = 0; while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; } return n.toFixed(i ? 1 : 0) + " " + u[i]; };
  const fmtRate = (b) => (!b || b < 1) ? "0 B/s" : fmtBytes(b) + "/s";
  const fmtDur = (sec) => { sec = Math.max(0, Math.floor(sec || 0)); const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60; return d ? `${d}天${h}时` : h ? `${h}时${m}分` : m ? `${m}分${s}秒` : `${s}秒`; };
  const ago = (ms) => ms ? fmtDur((Date.now() - ms) / 1000) + "前" : "-";
  const netNorm = (n) => { n = (n || "tcp").toLowerCase().replace(/\s/g, "").replace("+", ","); return n === "udp,tcp" ? "tcp,udp" : n; };
  const netLabel = (n) => NETS[netNorm(n)] || "TCP";
  const hostOf = (addr) => { const i = (addr || "").lastIndexOf(":"); return i > 0 ? addr.slice(0, i) : (addr || "?"); };
  const outKey = (a) => a.name || a.target;
  // SVG 文字不会自动截断：按估算宽度（中日韩字符 ≈ 1em，其他 ≈ 0.62em）截断并加省略号，完整内容看悬停提示
  const fitText = (s, maxPx, fs) => {
    s = String(s == null ? "" : s);
    let w = 0;
    for (let i = 0; i < s.length; i++) {
      w += (s.charCodeAt(i) >= 0x2e80 ? 1 : 0.62) * fs;
      if (w > maxPx) return s.slice(0, Math.max(0, i - 1)) + "…";
    }
    return s;
  };

  // 登录状态：任何接口返回 401 都回到登录页
  const auth = reactive({ checked: false, loggedIn: false, user: "", defaultPassword: false });
  async function api(path, opt) {
    const r = await fetch(path, Object.assign({ credentials: "same-origin" }, opt));
    const j = await r.json().catch(() => ({ error: r.statusText }));
    if (r.status === 401 && !path.startsWith("/api/login")) auth.loggedIn = false;
    if (!r.ok) throw new Error(j.error || r.statusText);
    return j;
  }
  const postJSON = (path, body) => api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  const gen = (kind, extra) => api("/api/gen?kind=" + kind + (extra || ""), { method: "POST" });
  async function copy(text) {
    try { await navigator.clipboard.writeText(text); }
    catch (e) { const t = document.createElement("textarea"); t.value = text; document.body.append(t); t.select(); document.execCommand("copy"); t.remove(); }
  }

  const newReality = () => ({ serverName: "", password: "", shortId: "", fingerprint: "chrome", spiderX: "", mldsa65Verify: "" });
  const newHealth = () => ({ enabled: false, type: "tcp", target: "", path: "/", insecure: false, anyStatus: false, interval: 30, timeout: 5, payload: "", expectReply: false });
  const newServer = () => ({ name: "", server: "", uuid: "", flow: "xtls-rprx-vision", tag: "", security: "reality", encryption: "none", reality: newReality(), disabled: false });

  // 服务端返回的配置 → 页面使用的完整结构
  function normalize(c) {
    c = clone(c || {});
    c.servers = (c.servers || []).map((s) => Object.assign(newServer(), s, { reality: Object.assign(newReality(), s.reality || {}) }));
    c.allow = (c.allow || []).map((a) => ({ name: a.name || "", target: a.target || "", network: netNorm(a.network), remotePort: a.remotePort || null, servers: a.servers || [],
      health: Object.assign(newHealth(), a.health || {}) }));
    c.memory = Object.assign({ limitMB: 0, gcPercent: 50, bufferKB: 16, maxProcs: 0, trimSec: 0 }, c.memory || {});
    c.logLevel = c.logLevel || "warning";
    return c;
  }
  // 页面结构 → 提交给后端的配置
  function toWire(c) {
    const w = { servers: [], allow: [], logLevel: c.logLevel, memory: clone(c.memory) };
    for (const s of c.servers) {
      const o = { name: s.name, server: s.server, uuid: s.uuid, flow: s.flow, security: s.security, encryption: s.encryption || "none" };
      if (s.tag) o.tag = s.tag; // 旧配置里手填过的保留；新建的留空由客户端自动生成
      if (s.disabled) o.disabled = true;
      if (s.testseed && s.testseed.length === 4) o.testseed = s.testseed;
      if (s.security === "reality") { o.reality = clone(s.reality); if (!o.reality.mldsa65Verify) delete o.reality.mldsa65Verify; }
      w.servers.push(o);
    }
    for (const a of c.allow) {
      const o = { target: a.target };
      if (a.name) o.name = a.name;
      if (netNorm(a.network) !== "tcp") o.network = netNorm(a.network);
      if (a.remotePort) o.remotePort = a.remotePort;
      if (a.servers && a.servers.length) o.servers = a.servers;
      const h = a.health || newHealth(), d = newHealth();
      if (JSON.stringify(h) !== JSON.stringify(d)) { // 关闭检测时也保留已填的设置
        const o2 = { enabled: !!h.enabled, type: h.type };
        for (const k of ["target", "path", "payload"]) if (h[k] && !(k === "path" && h[k] === "/")) o2[k] = h[k];
        for (const k of ["insecure", "anyStatus", "expectReply"]) if (h[k]) o2[k] = true;
        if (h.interval && h.interval !== 30) o2.interval = h.interval;
        if (h.timeout && h.timeout !== 5) o2.timeout = h.timeout;
        o.health = o2;
      }
      w.allow.push(o);
    }
    return w;
  }

  // ---------------- 分享链接 ----------------
  function parseLink(link) {
    link = (link || "").trim();
    if (!/^vless:\/\//i.test(link)) throw new Error("只支持 vless:// 链接");
    const u = new URL(link.replace(/^vless:/i, "http:"));
    const q = u.searchParams, uuid = decodeURIComponent(u.username);
    if (!uuid) throw new Error("链接中缺少 UUID");
    const type = (q.get("type") || "tcp").toLowerCase();
    if (type !== "tcp" && type !== "raw") throw new Error(`传输方式是 ${type}，xrayclient-reversepanel 只支持 RAW(tcp)`);
    if ((q.get("headerType") || "none").toLowerCase() !== "none") throw new Error("不支持 RAW 的 http 伪装头");
    const sec = (q.get("security") || "none").toLowerCase();
    if (sec !== "reality" && sec !== "none") throw new Error(`security 是 ${sec}，xrayclient-reversepanel 只支持 reality 或 none + encryption`);
    const enc = q.get("encryption") || "none";
    if (sec === "none" && enc === "none") throw new Error("security=none 且 encryption=none 是明文，xrayclient-reversepanel 不允许");
    return {
      server: u.hostname + ":" + (u.port || "443"), uuid, flow: q.get("flow") || "", security: sec, encryption: enc,
      reality: sec === "reality" ? { serverName: q.get("sni") || "", password: q.get("pbk") || q.get("password") || "", shortId: q.get("sid") || "", fingerprint: (q.get("fp") || "chrome").toLowerCase(), spiderX: q.get("spx") || "", mldsa65Verify: q.get("pqv") || "" } : null,
      remark: decodeURIComponent(u.hash.replace(/^#/, "")),
    };
  }
  function buildLink(s) {
    if (!s.server || !s.uuid) throw new Error("请先填写地址和 UUID");
    const q = new URLSearchParams({ type: "tcp", encryption: s.encryption || "none", security: s.security });
    if (s.flow) q.set("flow", s.flow);
    if (s.security === "reality") {
      const r = s.reality; q.set("sni", r.serverName); q.set("fp", r.fingerprint); q.set("pbk", r.password); q.set("sid", r.shortId);
      if (r.spiderX) q.set("spx", r.spiderX); if (r.mldsa65Verify) q.set("pqv", r.mldsa65Verify);
    }
    return `vless://${encodeURIComponent(s.uuid)}@${s.server}?${q.toString()}#${encodeURIComponent(s.name || s.tag || "xrayclient-reversepanel")}`;
  }

  // ---------------- 应用 ----------------
  const app = createApp({
    template: "#tpl",
    setup() {
      const pages = [
        { id: "dashboard", title: "概览" },
        { id: "servers", title: "服务端" },
        { id: "outlets", title: "出站" },
        { id: "system", title: "系统" },
      ];
      const page = ref((location.hash || "#dashboard").slice(1));
      window.addEventListener("hashchange", () => { page.value = (location.hash || "#dashboard").slice(1); });
      const go = (p) => { location.hash = p; page.value = p; };
      const navOpen = ref(false);

      const status = ref({ servers: [], targets: [] });
      const cfg = ref(normalize({}));
      const draft = ref(normalize({}));
      const logs = ref([]);
      const logbox = ref(null);
      const saving = ref(false);
      const hover = ref(null);
      const toasts = ref([]);
      const secrets = reactive({}); // 本页生成的服务端密钥：{服务端名称: {privateKey, decryption}}，只在内存中
      const encAuth = ref("x25519"), encMode = ref("native");
      const srvEdit = ref(null), outEdit = ref(null), snip = ref(null);

      let tid = 0;
      const toast = (text, ok = true) => { const id = ++tid; toasts.value.push({ id, text, ok }); setTimeout(() => { toasts.value = toasts.value.filter((t) => t.id !== id); }, ok ? 2500 : 6000); };

      // ---------- 实时速率 ----------
      const rates = reactive({ total: { up: 0, down: 0 }, servers: {}, targets: {} });
      const hist = reactive({ samples: [] }); // 每 2 秒一个点：{t, up, down, srv: {服务端: {up, down}}, hl: {出站: 健康状态}}
      const chartSel = ref(""), chartTip = ref(null), plotEl = ref(null);
      let prev = null;
      function computeRates(s) {
        const now = Date.now(), dt = prev ? (now - prev.t) / 1000 : 0;
        const r = (cur, old) => (dt > 0 && old != null && cur >= old) ? (cur - old) / dt : 0;
        const next = { t: now, rx: s.rx, tx: s.tx, srv: {}, tg: {} };
        rates.total = { up: r(s.tx, prev?.tx), down: r(s.rx, prev?.rx) };
        const sv = {}, tg = {};
        for (const x of s.servers || []) { next.srv[x.name] = { rx: x.rx, tx: x.tx }; sv[x.name] = { up: r(x.tx, prev?.srv[x.name]?.tx), down: r(x.rx, prev?.srv[x.name]?.rx) }; }
        for (const t of s.targets || []) {
          const k = t.server + "|" + t.addr; next.tg[k] = { rx: t.rx, tx: t.tx };
          tg[k] = { up: r(t.rx, prev?.tg[k]?.rx), down: r(t.tx, prev?.tg[k]?.tx) }; // rx=本地服务返回(上传) tx=发往本地服务(下载)
        }
        rates.servers = sv; rates.targets = tg;
        if (prev) {
          const hl = {}; for (const k in (s.health || {})) hl[k] = s.health[k].status;
          hist.samples.push({ t: now, up: rates.total.up, down: rates.total.down, srv: sv, hl });
          if (hist.samples.length > 60) hist.samples.shift();
        }
        prev = next;
      }
      const rateNow = computed(() => rates.total);

      async function poll() {
        if (!auth.loggedIn) return;
        try {
          const s = await api("/api/status");
          s.servers = Array.isArray(s.servers) ? s.servers : [];
          s.targets = Array.isArray(s.targets) ? s.targets : [];
          computeRates(s); status.value = s;
        }
        catch (e) { status.value = Object.assign({}, status.value, { state: "offline", detail: "管理接口不可用" }); }
        if (page.value === "system") {
          try {
            const box = logbox.value, atBottom = !box || box.scrollTop + box.clientHeight >= box.scrollHeight - 8;
            const l = await api("/api/logs");
            logs.value = Array.isArray(l) ? l : [];
            if (atBottom) nextTick(() => { if (logbox.value) logbox.value.scrollTop = logbox.value.scrollHeight; });
          } catch (e) { /* ignore */ }
        }
      }
      async function loadConfig() { const c = normalize(await api("/api/config")); cfg.value = c; draft.value = clone(c); }

      // ---------- 状态查询 ----------
      const sv = (s) => (status.value.servers || []).find((x) => x.name === s.name) || null;
      const applied = (s) => cfg.value.servers.some((x) => x.name === s.name);
      const srvState = (s) => { if (s.disabled) return "disabled"; if (!applied(s)) return "pending"; const v = sv(s); return v ? v.state : "stopped"; };
      const srvRate = (s) => rates.servers[s.name] || { up: 0, down: 0 };
      function outStat(a, serverName) {
        const key = outKey(a), o = { active: 0, tcp: netNorm(a.network).includes("tcp"), up: 0, down: 0, fails: 0, err: "", last: 0 };
        for (const t of status.value.targets || []) {
          if (t.name !== key || (serverName && t.server !== serverName)) continue;
          if (t.active > 0) o.active += t.active;
          const r = rates.targets[t.server + "|" + t.addr]; if (r) { o.up += r.up; o.down += r.down; }
          o.fails += t.fails;
          if (t.lastTime > o.last) { o.last = t.lastTime; o.err = t.lastErr; }
        }
        return o;
      }
      const healthText = (h) => !h ? "检测中" : h.status === "up" ? (h.noReply ? "未被拒绝" : `正常 ${h.latencyMs} ms`) : h.status === "down" ? "异常" : "检测中";
      const healthBadge = (h) => !h || h.status === "pending" ? "text-bg-secondary" : h.status === "up" ? "text-bg-success" : "text-bg-danger";
      // 最近 30 次检测的小柱状图：高度 = 延迟，红色 = 失败
      function spark(h) {
        const hist = (h && h.history) || [], max = Math.max(1, ...hist.filter((v) => v >= 0));
        return hist.map((v, k) => v < 0 ? { x: k * 3, y: 0, h: 16, bad: true } : { x: k * 3, y: 16 - Math.max(2, v / max * 16), h: Math.max(2, v / max * 16), bad: false });
      }
      const stateText = (s) => STATE_TEXT[s] || s || "-";
      const stateBadge = (s) => STATE_BADGE[s] || "text-bg-secondary";

      // ---------- 概览 ----------
      const tiles = computed(() => {
        const s = status.value, list = s.servers || [];
        const enabled = list.filter((x) => x.state !== "disabled"), online = enabled.filter((x) => x.state === "online");
        return [
          { k: "在线服务端", v: `${online.length} / ${enabled.length}`, s: stateText(s.state), cls: online.length && online.length === enabled.length ? "text-success" : online.length ? "text-warning" : "text-danger", tip: "在线 / 已启用的服务端数量" },
          { k: "当前连接", v: s.active ?? 0, s: "累计 " + (s.conns ?? 0), tip: "全部出站当前的 TCP 连接数（UDP 不计）/ 自程序启动累计连接数" },
          { k: "实时上传", v: fmtRate(rates.total.up), s: "总上传 " + fmtBytes(s.tx), cls: "text-primary", tip: "发往服务端，全部服务端合计（含加密与填充开销）" },
          { k: "实时下载", v: fmtRate(rates.total.down), s: "总下载 " + fmtBytes(s.rx), cls: "text-success", tip: "从服务端收到，全部服务端合计" },
          { k: "实际内存", v: s.rssMB ? s.rssMB + " MB" : (s.sysMB || 0) + " MB", s: `Go 堆 ${s.heapMB ?? 0} MB · 协程 ${s.goroutines ?? 0}`, tip: "进程常驻内存（RSS），与 top / ps 看到的一致" },
          { k: "运行时长", v: fmtDur(s.uptime), s: `CPU 线程 ${s.maxProcs ?? "-"}`, tip: "自程序启动" },
        ];
      });
      // ---------- 实时流量曲线 ----------
      const N = 60; // 2 分钟，每 2 秒一个点
      const PALETTE = ["#3b82f6", "#20c05c", "#f5a524", "#a855f7", "#ef4444", "#14b8a6", "#ec4899", "#64748b"];
      // 让刻度落在 1/2/2.5/5 × 10ⁿ 的整齐数值上
      // 刻度在显示单位（B/KB/MB/GB，1024 进制）里取 1/2/2.5/5 × 10ⁿ，标签就会是 500 KB/s、1000 KB/s 这样的整数
      const UNITS = ["B/s", "KB/s", "MB/s", "GB/s"];
      function niceStep(max) {
        const raw = max / 4; let k = 0; while (k < 3 && raw >= 1024 ** (k + 1)) k++;
        const r = raw / 1024 ** k, p = Math.pow(10, Math.floor(Math.log10(r)));
        let m = 10; for (const c of [1, 2, 2.5, 5, 10]) if (c * p >= r) { m = c; break; }
        return { step: m * p * 1024 ** k, unit: k };
      }
      const chart = computed(() => {
        const S = hist.samples, L = S.length, sel = chartSel.value;
        let series;
        if (sel === "__cmp") {
          series = cfg.value.servers.map((s, i) => ({ key: s.name, label: s.name, color: PALETTE[i % PALETTE.length], vals: S.map((x) => (x.srv[s.name]?.up || 0) + (x.srv[s.name]?.down || 0)) }));
        } else {
          const pick = sel ? (x) => x.srv[sel] || { up: 0, down: 0 } : (x) => x;
          series = [
            { key: "up", label: (sel ? sel + " " : "") + "上传", color: PALETTE[0], vals: S.map((x) => pick(x).up || 0) },
            { key: "down", label: (sel ? sel + " " : "") + "下载", color: PALETTE[1], vals: S.map((x) => pick(x).down || 0) },
          ];
        }
        const peak = Math.max(0, ...series.flatMap((s) => s.vals));
        const ns = niceStep(Math.max(peak, 1024)), step = ns.step, top = step * 4;
        const yOf = (v) => 158 - v / top * 150;
        const xOf = (i) => (i + N - L) * 600 / (N - 1);
        for (const s of series) {
          s.points = s.vals.map((v, i) => `${xOf(i).toFixed(1)},${yOf(v).toFixed(1)}`).join(" ");
          s.now = s.vals.length ? s.vals[s.vals.length - 1] : 0;
        }
        const fmtTick = (v) => { const n = v / 1024 ** ns.unit; return (Number.isInteger(n) ? n : +n.toFixed(1)) + " " + UNITS[ns.unit]; };
        const ticks = [0, 1, 2, 3, 4].map((k) => ({ v: k, y: yOf(k * step), pct: yOf(k * step) / 160 * 100, label: k ? fmtTick(k * step) : "0" }));
        return { series, ticks, peak, top, yOf };
      });
      function chartHover(ev) {
        const box = plotEl.value?.getBoundingClientRect(); if (!box) return;
        const slot = Math.round(Math.min(1, Math.max(0, (ev.clientX - box.left) / box.width)) * (N - 1));
        chartTip.value = { slot };
      }
      const tipView = computed(() => {
        if (!chartTip.value) return null;
        const S = hist.samples, idx = chartTip.value.slot - (N - S.length);
        if (idx < 0 || idx >= S.length) return null;
        const c = chart.value, x = chartTip.value.slot * 600 / (N - 1), smp = S[idx];
        const down = Object.entries(smp.hl || {}).filter(([, st]) => st === "down").map(([k]) => k);
        return { x, xp: x / 6, time: new Date(smp.t).toLocaleTimeString(), down,
          dots: c.series.map((s) => ({ key: s.key, label: s.label, color: s.color, v: s.vals[idx], yp: c.yOf(s.vals[idx]) / 160 * 100 })) };
      });
      // 健康检测时间线：每个开启检测的出站一行，按采样点着色，和曲线共用时间轴
      const healthRows = computed(() => {
        const S = hist.samples, L = S.length, w = 600 / N;
        return cfg.value.allow.filter((a) => a.health && a.health.enabled).map((a) => {
          const k = outKey(a);
          return { key: k, cells: S.map((x, i) => ({ x: (i + N - L) * w, w: w + 0.3, s: x.hl[k] === "up" ? "up" : x.hl[k] === "down" ? "down" : "na" })) };
        });
      });

      // ---------- 拓扑图（使用已生效的配置）：出站对象 → 本机客户端 → 服务端 → 服务端入口 ----------
      const hv = (a) => (status.value.health || {})[outKey(a)] || null;
      // 出站对象 ↔ 本体 这段链路：健康检测失败 = 断线；有流量 = 传输中；检测正常 = 空闲；没开检测又没数据 = 未监测
      function outLink(a) {
        const h = a.health && a.health.enabled ? hv(a) : null, x = outStat(a);
        if (h && h.status === "down") return "down";
        if (x.active > 0 || x.up + x.down > BUSY_BPS) return "busy";
        if (h && h.status === "up") return "idle";
        if (x.err) return "down";
        return "unknown";
      }
      const LINK = { busy: "传输中", idle: "空闲（链路正常，暂无数据）", down: "断线", disabled: "已停用", unknown: "未开启健康检测，暂无数据" };
      const topo = computed(() => {
        const c = cfg.value, s0 = status.value, NW = 196, NH = 46, GAP = 14, cols = [6, 300, 594, 888], W = 1090;
        const nodes = [], edges = [];
        const servers = c.servers, outlets = c.allow;
        const allowedFor = (o, s) => !o.servers || !o.servers.length || o.servers.includes(s.name);
        const entr = [];
        servers.forEach((s, si) => outlets.forEach((o, oi) => { if (allowedFor(o, s)) entr.push({ s, si, o, oi }); }));
        const rows = Math.max(outlets.length, servers.length, entr.length, 1);
        const H = 30 + rows * (NH + GAP) + 6;
        const ys = (count, i) => 30 + ((rows - count) * (NH + GAP)) / 2 + i * (NH + GAP);
        const yc = ys(1, 0) + NH / 2; // 本体居中
        const curve = (x1, y1, x2, y2) => `M${x1},${y1} C${x1 + 70},${y1} ${x2 - 70},${y2} ${x2},${y2}`;
        const chains = {};
        const addChain = (id, set) => { (chains[id] = chains[id] || new Set()); set.forEach((x) => chains[id].add(x)); return chains[id]; };
        const srvLink = (s) => { const st = srvState(s), r = srvRate(s); return st === "disabled" ? "disabled" : st !== "online" ? "down" : (r.up + r.down > BUSY_BPS ? "busy" : "idle"); };
        const pairLink = (s, o) => {
          const st = srvState(s); if (st === "disabled") return "disabled"; if (st !== "online") return "down";
          const x = outStat(o, s.name); return (x.active > 0 || x.up + x.down > BUSY_BPS) ? "busy" : "idle";
        };

        // 整条链路：出站对象 ⇄ 本体 ⇄ 服务端 ⇄ 入口
        entr.forEach((e, ei) => {
          const oId = "o" + e.oi, sId = "s" + e.si, eId = "e" + ei, se = "se" + ei, oc = "oc" + e.oi, cs = "cs" + e.si;
          const chain = new Set([oId, oc, "core", cs, sId, se, eId]);
          addChain(oId, chain); addChain(sId, chain); addChain("core", chain);
          const st = pairLink(e.s, e.o), x = outStat(e.o, e.s.name);
          const tip = {
            title: `${outKey(e.o)} ⇄ ${e.s.name} ⇄ ${hostOf(e.s.server)}:${e.o.remotePort || "?"}`,
            state: st, stateText: LINK[st],
            lines: [`当前连接：${x.tcp ? x.active : "-（UDP 不统计）"}`, `实时 上传 ${fmtRate(x.up)} / 下载 ${fmtRate(x.down)}`, ...(x.err ? ["最近错误：" + x.err] : [])],
            hint: st === "down" ? "服务端离线或尚未连上" : st === "disabled" ? "该服务端已停用" : "",
          };
          const ye = ys(entr.length, ei) + NH / 2, yss = ys(servers.length, e.si) + NH / 2;
          edges.push({ id: se, d: curve(cols[2] + NW, yss, cols[3], ye), state: st, tip, chain });
          nodes.push({ id: eId, kind: "entr", state: st, x: cols[3], y: ye - NH / 2, l1: e.o.remotePort ? `${hostOf(e.s.server)}:${e.o.remotePort}` : "未设置对外端口",
            l2: `${outKey(e.o)} · ${netLabel(e.o.network)}`, tip, chain, ref: { type: "entr", si: e.si } });
        });

        // 出站对象（本地服务）及其到本体的链路（颜色反映健康检测）
        outlets.forEach((o, oi) => {
          const id = "o" + oi, oc = "oc" + oi, chain = addChain(id, new Set([id, oc, "core"]));
          addChain("core", chain);
          const st = outLink(o), x = outStat(o), on = o.health && o.health.enabled, h = on ? hv(o) : null;
          const lines = [`当前连接：${x.tcp ? x.active : "-（UDP 不统计）"}　失败：${x.fails}`, `实时 上传 ${fmtRate(x.up)} / 下载 ${fmtRate(x.down)}`];
          if (on) {
            lines.push(`健康检测：${o.health.type.toUpperCase()} ${h ? h.target : ""}`);
            lines.push((!h || h.status === "pending") ? "检测中…" : h.status === "up" ? (h.noReply ? `未收到回复但也没被拒绝（UDP 无法完全确认，${ago(h.lastCheck)}）` : `正常，延迟 ${h.latencyMs} ms（${ago(h.lastCheck)}）`) : `异常：${h.lastErr}（${ago(h.lastCheck)}）`);
          } else lines.push("健康检测：未开启");
          if (x.err) lines.push("最近错误：" + x.err);
          const tip = { title: `${o.name || o.target}（${o.target} · ${netLabel(o.network)}）`, state: st === "unknown" ? "disabled" : st,
            stateText: h && h.status === "down" ? "健康检测失败" : LINK[st], lines };
          const y = ys(outlets.length, oi);
          edges.push({ id: oc, d: curve(cols[0] + NW, y + NH / 2, cols[1], yc), state: st, tip, chain });
          nodes.push({ id, kind: "out", state: { down: "err", busy: "busy", idle: "ok", unknown: "unknown" }[st], x: cols[0], y,
            l1: o.name || o.target, l2: `${o.target} · ${netLabel(o.network)}${h && h.status === "up" && !h.noReply ? " · " + h.latencyMs + " ms" : ""}`,
            tip, chain, ref: { type: "out", key: outKey(o) } });
        });

        // 服务端及其到本体的链路
        servers.forEach((s, si) => {
          const id = "s" + si, cs = "cs" + si, chain = addChain(id, new Set([id, cs, "core"]));
          addChain("core", chain);
          const st = srvState(s), v = sv(s), r = srvRate(s), ls = srvLink(s);
          const tip = {
            title: `${s.name}（${s.server || "-"}）`, state: ls,
            stateText: stateText(st) + (v && v.detail ? "：" + v.detail : ""),
            lines: [`隧道连接：${v ? v.tunnels : "-"}　已连接：${v && v.since ? fmtDur((Date.now() - v.since) / 1000) : "-"}`,
              `实时 上传 ${fmtRate(r.up)} / 下载 ${fmtRate(r.down)}`, `累计 上传 ${fmtBytes(v?.tx)} / 下载 ${fmtBytes(v?.rx)}`],
          };
          const y = ys(servers.length, si);
          edges.push({ id: cs, d: curve(cols[1] + NW, yc, cols[2], y + NH / 2), state: ls, tip, chain });
          nodes.push({ id, kind: "srv", state: st, x: cols[2], y, l1: s.name, l2: `${s.server || "-"} · ${stateText(st)}`, tip, chain, ref: { type: "srv", name: s.name } });
        });

        // xrayclient-reversepanel 本体
        const coreChain = chains["core"] || new Set(["core"]);
        nodes.push({ id: "core", kind: "core", state: s0.state || "stopped", x: cols[1], y: yc - NH / 2, l1: "本机客户端",
          l2: `${s0.rssMB ? s0.rssMB + " MB" : (s0.sysMB || 0) + " MB"} · 连接 ${s0.active ?? 0}`,
          tip: { title: "本机客户端", state: s0.state === "online" ? "idle" : s0.state === "stopped" ? "disabled" : "down", stateText: stateText(s0.state) + (s0.detail ? "：" + s0.detail : ""),
            lines: [`实时 上传 ${fmtRate(rates.total.up)} / 下载 ${fmtRate(rates.total.down)}`, `累计 上传 ${fmtBytes(s0.tx)} / 下载 ${fmtBytes(s0.rx)}`,
              `内存 ${s0.rssMB || s0.sysMB || 0} MB · Go 堆 ${s0.heapMB ?? 0} MB · 协程 ${s0.goroutines ?? 0}`, `CPU 线程 ${s0.maxProcs ?? "-"} · 运行 ${fmtDur(s0.uptime)}`] },
          chain: coreChain, ref: { type: "core" } });
        return { nodes, edges, w: W, h: H, nw: NW, nh: NH, cols, outlets, servers };
      });
      // 悬停信息面板：只记录悬停对象和鼠标位置，内容每次从最新的 topo 里取，所以会随轮询实时刷新
      const tip = ref(null), topoWrap = ref(null);
      function showTip(ev, id) {
        const box = topoWrap.value?.getBoundingClientRect(); if (!box) return;
        let x = ev.clientX - box.left + 14, y = ev.clientY - box.top + 14;
        if (x > box.width - 300) x = Math.max(4, ev.clientX - box.left - 314);
        tip.value = { id, x, y };
      }
      const tipItem = computed(() => {
        if (!tip.value) return null;
        const t = topo.value, it = t.edges.find((e) => e.id === tip.value.id) || t.nodes.find((n) => n.id === tip.value.id);
        return it ? it.tip : null;
      });
      function openNode(n) {
        if (n.ref.type === "out") { go("outlets"); const i = draft.value.allow.findIndex((a) => outKey(a) === n.ref.key); if (i >= 0) editOutlet(i); }
        else if (n.ref.type === "srv") { go("servers"); const i = draft.value.servers.findIndex((s) => s.name === n.ref.name); if (i >= 0) editServer(i); }
        else if (n.ref.type === "core") go("system");
        else { go("servers"); const name = cfg.value.servers[n.ref.si]?.name; const i = draft.value.servers.findIndex((s) => s.name === name); if (i >= 0) showSnippet(i); }
      }

      // ---------- 未保存的更改 ----------
      const changes = computed(() => {
        const a = cfg.value, b = draft.value, out = [];
        const byName = (list) => Object.fromEntries(list.map((x) => [x.name, JSON.stringify(x)]));
        const sa = byName(a.servers), sb = byName(b.servers);
        for (const n in sb) { if (!(n in sa)) out.push(`添加服务端「${n}」`); else if (sa[n] !== sb[n]) out.push(`修改服务端「${n}」`); }
        for (const n in sa) if (!(n in sb)) out.push(`删除服务端「${n}」`);
        const ka = Object.fromEntries(a.allow.map((x) => [outKey(x), JSON.stringify(x)])), kb = Object.fromEntries(b.allow.map((x) => [outKey(x), JSON.stringify(x)]));
        for (const n in kb) { if (!(n in ka)) out.push(`添加出站「${n}」`); else if (ka[n] !== kb[n]) out.push(`修改出站「${n}」`); }
        for (const n in ka) if (!(n in kb)) out.push(`删除出站「${n}」`);
        if (JSON.stringify(a.memory) !== JSON.stringify(b.memory) || a.logLevel !== b.logLevel) out.push("修改运行参数");
        if (!out.length && JSON.stringify(toWire(a)) !== JSON.stringify(toWire(b))) out.push("调整顺序");
        return out;
      });
      const dirty = computed(() => changes.value.length > 0);
      const changeCount = computed(() => changes.value.length);
      window.addEventListener("beforeunload", (e) => { if (dirty.value) e.preventDefault(); });

      async function saveApply() {
        saving.value = true;
        try {
          const r = await api("/api/config", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(toWire(draft.value)) });
          await loadConfig(); toast(r.ok || "已应用");
          if (page.value === "changes") go("dashboard");
        } catch (e) { toast(e.message, false); }
        saving.value = false; poll();
      }
      function revert() { draft.value = clone(cfg.value); toast("已放弃更改"); }
      async function restart() { try { const r = await api("/api/restart", { method: "POST" }); toast(r.ok); } catch (e) { toast(e.message, false); } poll(); }

      // ---------- 服务端编辑 ----------
      function editServer(i) {
        const s = i < 0 ? newServer() : clone(draft.value.servers[i]);
        s.reality = Object.assign(newReality(), s.reality || {});
        if (i < 0) s.name = "服务端" + (draft.value.servers.length + 1);
        srvEdit.value = { index: i, s, oldName: i < 0 ? null : s.name, seed: (s.testseed || ["", "", "", ""]).map(String), link: "", err: "", secret: Object.assign({}, secrets[s.name] || {}) };
      }
      const openImport = () => editServer(-1);
      function fillFromLink(ed) {
        try {
          const p = parseLink(ed.link), s = ed.s;
          Object.assign(s, { server: p.server, uuid: p.uuid, flow: p.flow, security: p.security, encryption: p.encryption });
          if (p.reality) s.reality = Object.assign(newReality(), p.reality);
          if (p.remark && (!s.name || /^服务端\d+$/.test(s.name))) s.name = p.remark;
          ed.link = ""; ed.err = ""; toast("已解析");
        } catch (e) { ed.err = "解析失败：" + e.message; }
      }
      async function genReality(ed) { try { const r = await gen("x25519"); ed.s.reality.password = r.password; ed.secret.privateKey = r.privateKey; toast("已生成 REALITY 密钥对，私钥会写进该服务端的配置片段"); } catch (e) { ed.err = e.message; } }
      async function genEnc(ed) { try { const r = await gen("vlessenc", "&auth=" + encAuth.value + "&mode=" + encMode.value); ed.s.encryption = r.encryption; ed.secret.decryption = r.decryption; toast("已生成 enc 密钥对，decryption 会写进该服务端的配置片段"); } catch (e) { ed.err = e.message; } }
      function commitServer() {
        const ed = srvEdit.value, s = ed.s, list = draft.value.servers;
        const others = list.filter((_, i) => i !== ed.index);
        if (!s.name) return (ed.err = "请填写名称");
        if (others.some((x) => x.name === s.name)) return (ed.err = "名称已被其他服务端使用");
        if (!/^.+:\d+$/.test(s.server)) return (ed.err = "地址需要 host:port");
        if (!s.uuid) return (ed.err = "请填写 UUID");
        if (s.security === "none" && (!s.encryption || s.encryption === "none")) return (ed.err = "无 TLS 时必须填写 encryption");
        const seed = ed.seed.map((v) => String(v).trim());
        if (seed.some((v) => v !== "")) s.testseed = seed.map((v, k) => v === "" ? [900, 500, 900, 256][k] : parseInt(v, 10)); else delete s.testseed;
        if (ed.index < 0) list.push(s); else list.splice(ed.index, 1, s);
        if (ed.oldName && ed.oldName !== s.name) { // 改名：同步出站里的引用
          for (const a of draft.value.allow) a.servers = (a.servers || []).map((n) => n === ed.oldName ? s.name : n);
          delete secrets[ed.oldName];
        }
        if (ed.secret.privateKey || ed.secret.decryption) secrets[s.name] = ed.secret;
        srvEdit.value = null;
      }
      function removeServer(i) {
        const s = draft.value.servers[i];
        if (!confirm(`删除服务端「${s.name}」？`)) return;
        draft.value.servers.splice(i, 1);
        const stale = draft.value.allow.filter((a) => (a.servers || []).includes(s.name));
        for (const a of stale) a.servers = a.servers.filter((n) => n !== s.name);
        const orphan = stale.filter((a) => !a.servers.length);
        if (orphan.length) { // 避免“只允许这个服务端”的出站被扩大成“全部服务端”
          for (const a of orphan) a.servers = [s.name];
          toast(`出站 ${orphan.map(outKey).join("、")} 只允许「${s.name}」，请先修改它们允许的服务端，否则无法保存`, false);
        }
      }
      async function copyLink(s) { try { await copy(buildLink(s)); toast("分享链接已复制"); } catch (e) { toast(e.message, false); } }

      function showSnippet(i) {
        const s = draft.value.servers[i], sec = secrets[s.name] || {};
        const port = parseInt((s.server || "").split(":").pop(), 10) || 443;
        // 服务端的 reverse tag 只需在服务端配置内部一致（client ↔ 路由规则）。用 UUID 前 8 位生成，
        // 多台客户端连同一个服务端时也不会撞名。
        const stag = "r-" + ((s.uuid || "").replace(/-/g, "").slice(0, 8) || "home");
        const user = { id: s.uuid || "<UUID>" }; if (s.flow) user.flow = "xtls-rprx-vision"; user.reverse = { tag: stag };
        const useEnc = s.encryption && s.encryption !== "none";
        const inbound = {
          tag: "in-" + stag, port, protocol: "vless",
          settings: { decryption: useEnc ? (sec.decryption || "<与客户端 encryption 配对的 decryption>") : "none", clients: [user] },
          streamSettings: s.security === "reality"
            ? { network: "raw", security: "reality", realitySettings: { target: (s.reality.serverName || "www.example.com") + ":443", serverNames: [s.reality.serverName || "www.example.com"], privateKey: sec.privateKey || "<与公钥配对的 privateKey>", shortIds: [s.reality.shortId || ""] } }
            : { network: "raw" },
        };
        const maps = [], tags = [];
        draft.value.allow.forEach((a, k) => {
          if (a.servers && a.servers.length && !a.servers.includes(s.name)) return;
          const m = a.target.match(/^\[?([^\]\/]+?)\]?(?:\/\d+)?:(\d+)/); if (!m) return;
          const t = "map-" + (a.name || k + 1).toString().replace(/[^\w-]/g, "_");
          tags.push(t);
          maps.push({ tag: t, port: a.remotePort || "<对外端口>", protocol: "tunnel", settings: { address: m[1], port: parseInt(m[2], 10), network: netNorm(a.network) } });
        });
        snip.value = {
          name: s.name,
          text: JSON.stringify({ inbounds: [inbound, ...maps], routing: { rules: [{ inboundTag: tags, outboundTag: stag }] } }, null, 2),
          hint: (sec.privateKey || sec.decryption) ? "已包含本页生成的服务端密钥（只在当前页面内存中，刷新就没了，请及时复制）。" : "privateKey / decryption 需填服务端自己的；在编辑里点“生成密钥对”会自动填入。",
        };
      }

      // ---------- 出站编辑 ----------
      function editOutlet(i) {
        const a = i < 0 ? { name: "", target: "", network: "tcp", remotePort: null, servers: [], health: newHealth() } : clone(draft.value.allow[i]);
        a.health = Object.assign(newHealth(), a.health || {});
        outEdit.value = { index: i, a, all: !a.servers || !a.servers.length, sel: [...(a.servers || [])], err: "" };
      }
      // 打开健康检测时按出站猜一个合适的检测方式
      function suggestHealth(ed) {
        const h = ed.a.health; if (!h.enabled || ed.touched) return;
        const port = parseInt((ed.a.target.match(/:(\d+)(-\d+)?$/) || [])[1], 10);
        if (netNorm(ed.a.network) === "udp") h.type = "udp";
        else if (port === 443 || port === 8443) { h.type = "https"; h.insecure = true; }
        else if (port === 80 || port === 8080) h.type = "http";
        else h.type = "tcp";
      }
      function commitOutlet() {
        const ed = outEdit.value, a = ed.a, list = draft.value.allow;
        if (!/^\[?[0-9a-fA-F.:]+\]?(\/\d+)?:\d+(-\d+)?$/.test(a.target || "")) return (ed.err = "本地目标格式应为 IP[/前缀]:端口[-端口]");
        if (list.some((x, i) => i !== ed.index && outKey(x) === outKey(a))) return (ed.err = "已有同名（或同目标）的出站");
        if (a.remotePort && (a.remotePort < 1 || a.remotePort > 65535)) return (ed.err = "对外端口无效");
        if (!ed.all && !ed.sel.length) return (ed.err = "至少选择一个服务端，或勾选“全部服务端”");
        const h = a.health;
        if (h.enabled) {
          const single = /^\[?[0-9a-fA-F.:]+\]?:\d+$/.test(a.target);
          if (!h.target && !single) return (ed.err = "出站是网段或端口范围，请在健康检测里填一个检测地址");
          if (h.target && !/^.+:\d+$/.test(h.target)) return (ed.err = "检测地址需要 host:端口");
          if (h.interval && h.interval < 5) return (ed.err = "检测间隔不能小于 5 秒");
          if (h.type === "udp" && h.payload.startsWith("hex:") && !/^hex:([0-9a-fA-F]{2})*$/.test(h.payload)) return (ed.err = "UDP 探测内容的十六进制格式不对");
        }
        a.servers = ed.all ? [] : [...ed.sel];
        a.network = netNorm(a.network);
        if (ed.index < 0) list.push(a); else list.splice(ed.index, 1, a);
        outEdit.value = null;
      }

      // ---------- 登录 / 修改密码 ----------
      const loginForm = reactive({ user: "admin", password: "", err: "", busy: false });
      const pwForm = reactive({ user: "", old: "", new1: "", new2: "", err: "", busy: false });
      async function afterLogin() {
        const s = await api("/api/session");
        Object.assign(auth, { checked: true, loggedIn: !!s.loggedIn, user: s.user || "", defaultPassword: !!s.defaultPassword });
        if (auth.loggedIn) { pwForm.user = auth.user; prev = null; hist.samples.length = 0; await loadConfig(); poll(); loadWeb().catch(() => {}); }
      }

      // ---------- 管理网页监听 ----------
      const webInfo = ref({});
      const webForm = reactive({ host: "", port: 7400, err: "", busy: false });
      const splitListen = (l) => { const i = (l || "").lastIndexOf(":"); return i < 0 ? ["", +l || 7400] : [l.slice(0, i).replace(/^\[|\]$/g, ""), +l.slice(i + 1)]; };
      const joinListen = (h, p) => (h.includes(":") ? "[" + h + "]" : h) + ":" + p;
      async function loadWeb() {
        webInfo.value = await api("/api/web");
        [webForm.host, webForm.port] = splitListen(webInfo.value.listen);
      }
      const webChanged = computed(() => webInfo.value.listen !== undefined && joinListen(webForm.host, webForm.port) !== webInfo.value.listen);
      async function saveWeb() {
        webForm.err = "";
        const p = webForm.port;
        if (!Number.isInteger(p) || p < 1 || p > 65535) return (webForm.err = "端口需要在 1–65535 之间");
        const host = webForm.host || "0.0.0.0";
        if (!["0.0.0.0", "::", "127.0.0.1", "::1", "localhost"].includes(host) && !confirm("只监听 " + host + "，确认这是本机的地址？填错会导致网页打不开（可用 xrpanel set-listen 改回）")) return;
        webForm.busy = true;
        try {
          const r = await api("/api/web", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ listen: joinListen(host, p) }) });
          toast(r.ok + "，正在跳转…");
          const target = ["0.0.0.0", "::", ""].includes(host) ? location.hostname : host;
          const hostPart = target.includes(":") && !target.startsWith("[") ? "[" + target + "]" : target;
          setTimeout(() => { location.href = "https://" + hostPart + ":" + p + "/" + location.hash; }, 1200);
        } catch (e) { webForm.err = e.message; webForm.busy = false; }
      }
      async function doLogin() {
        loginForm.busy = true; loginForm.err = "";
        try { await postJSON("/api/login", { user: loginForm.user, password: loginForm.password }); loginForm.password = ""; await afterLogin(); }
        catch (e) { loginForm.err = e.message; }
        loginForm.busy = false;
      }
      async function doLogout() {
        if (dirty.value && !confirm("有未保存的更改，确定退出？")) return;
        try { await postJSON("/api/logout", {}); } catch (e) { /* ignore */ }
        auth.loggedIn = false; draft.value = clone(cfg.value);
      }
      async function changePassword() {
        pwForm.err = "";
        if (pwForm.new1 !== pwForm.new2) return (pwForm.err = "两次输入的新密码不一致");
        pwForm.busy = true;
        try {
          const r = await postJSON("/api/password", { old: pwForm.old, user: pwForm.user, new: pwForm.new1 });
          pwForm.old = pwForm.new1 = pwForm.new2 = ""; toast(r.ok); await afterLogin();
        } catch (e) { pwForm.err = e.message; }
        pwForm.busy = false;
      }

      // ---------- 启动 ----------
      afterLogin().catch((e) => { auth.checked = true; toast(e.message, false); });
      setInterval(() => { if (!document.hidden) poll(); }, 2000);

      return {
        auth, loginForm, pwForm, doLogin, doLogout, changePassword, webInfo, webForm, webChanged, saveWeb,
        pages, page, go, navOpen, status, cfg, draft, logs, logbox, saving, hover, toasts, encAuth, encMode, srvEdit, outEdit, snip, fps: FPS,
        tiles, chart, rateNow, chartSel, chartTip, plotEl, chartHover, tipView, healthRows, topo, changes, dirty, changeCount,
        sv, srvState, srvRate, outStat, stateText, stateBadge, netLabel,
        fmtBytes, fmtRate, fmtDur, ago, gen, copy, toast, fitText,
        saveApply, revert, restart, openNode, tip, topoWrap, showTip, tipItem,
        editServer, openImport, fillFromLink, genReality, genEnc, commitServer, removeServer, copyLink, showSnippet,
        editOutlet, commitOutlet, suggestHealth, hv, healthText, healthBadge, spark,
      };
    },
  });
  // 任何渲染错误都只提示，不让整页白屏
  app.config.errorHandler = (err, inst, info) => {
    console.error("[xrayclient-reversepanel]", info, err);
    let box = document.getElementById("xl-err");
    if (!box) { box = document.createElement("div"); box.id = "xl-err"; box.className = "alert alert-danger position-fixed bottom-0 end-0 m-3 small shadow"; box.style.zIndex = 3000; box.style.maxWidth = "420px"; document.body.append(box); }
    box.textContent = "页面出错（" + info + "）：" + (err && err.message || err) + "。刷新页面通常可以恢复，如反复出现请把这段文字发给开发者。";
  };
  app.mount("#app");
})();
