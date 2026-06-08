/* aimon dashboard — live cards, gauges, sparklines, SSE, OAuth modal */
(() => {
  "use strict";

  const GAUGE_CIRC = 2 * Math.PI * 52; // r=52 in the 120x120 viewBox

  // Per-provider visual identity.
  const PROVIDERS = {
    deepseek:   { icon: "DS", c1: "#5b76ff", c2: "#8b5bff" },
    openrouter: { icon: "OR", c1: "#22d3ee", c2: "#3b82f6" },
    anthropic:  { icon: "AN", c1: "#d97757", c2: "#f59e0b" },
    claude:     { icon: "C",  c1: "#d97757", c2: "#c0623f" },
    xai:        { icon: "𝕏",  c1: "#cbd5e1", c2: "#7c8aa3" },
  };

  const pollInterval = Math.max(5, parseInt(document.body.dataset.poll, 10) || 60);
  const cardsEl = document.getElementById("cards");
  const emptyEl = document.getElementById("empty");
  const tpl = document.getElementById("card-tpl");

  const cards = new Map(); // id -> { root, refs, value }
  let lastRefresh = Date.now();

  // ---------- formatting ----------
  const fmtMoney = (n, unit) => {
    const sym = { USD: "$", CNY: "¥", EUR: "€", GBP: "£" }[unit];
    const v = Math.abs(n) >= 1000 ? n.toFixed(0) : n.toFixed(2);
    if (sym) return sym + Number(v).toLocaleString();
    return Number(v).toLocaleString() + (unit ? " " + unit : "");
  };
  const fmtNum = (n) => (Math.round(n * 10) / 10).toLocaleString();

  const relTime = (sec) => {
    if (!sec) return "—";
    const d = Math.max(0, Math.floor(Date.now() / 1000 - sec));
    if (d < 5) return "just now";
    if (d < 60) return d + "s ago";
    if (d < 3600) return Math.floor(d / 60) + "m ago";
    if (d < 86400) return Math.floor(d / 3600) + "h ago";
    return Math.floor(d / 86400) + "d ago";
  };

  const absTime = (sec) =>
    new Date(sec * 1000).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });

  // Mirror of model.Status.Remaining(): fraction of credit/quota left, or null.
  function remainingFraction(s) {
    if (s.balance != null && s.total != null && s.total > 0) return clamp01(s.balance / s.total);
    if (s.used != null && s.total != null && s.total > 0) return clamp01(1 - s.used / s.total);
    return null;
  }
  const clamp01 = (f) => Math.min(1, Math.max(0, f));

  // ---------- count-up animation ----------
  function countUp(node, from, to, fmt) {
    const dur = 650, t0 = performance.now();
    if (!isFinite(from)) from = to;
    function frame(t) {
      const p = Math.min(1, (t - t0) / dur);
      const e = 1 - Math.pow(1 - p, 3); // easeOutCubic
      node.textContent = fmt(from + (to - from) * e);
      if (p < 1) requestAnimationFrame(frame);
    }
    requestAnimationFrame(frame);
  }

  // ---------- card lifecycle ----------
  function makeCard(id) {
    const root = tpl.content.firstElementChild.cloneNode(true);
    const refs = {
      root,
      icon: root.querySelector(".prov-icon"),
      label: root.querySelector(".label"),
      kind: root.querySelector(".kind"),
      dot: root.querySelector(".status-dot"),
      gaugeWrap: root.querySelector(".gauge-wrap"),
      fill: root.querySelector(".gauge-fill"),
      bigVal: root.querySelector(".big-value"),
      bigUnit: root.querySelector(".big-unit"),
      metaPrimary: root.querySelector(".meta-line.primary"),
      metaSecondary: root.querySelector(".meta-line.secondary"),
      spark: root.querySelector(".spark"),
      sparkLine: root.querySelector(".spark-line"),
      updated: root.querySelector(".updated"),
      connect: root.querySelector(".btn-connect"),
    };
    cards.set(id, { root, refs, value: NaN, updatedAt: 0 });
    cardsEl.appendChild(root);
    return cards.get(id);
  }

  function render(list) {
    if (!list || list.length === 0) {
      emptyEl.hidden = cards.size > 0;
      return;
    }
    emptyEl.hidden = true;
    const seen = new Set();

    list.forEach((s, i) => {
      const id = s.provider + "/" + s.label;
      seen.add(id);
      let c = cards.get(id);
      if (!c) {
        c = makeCard(id);
        c.root.style.animationDelay = i * 70 + "ms";
        fetchHistory(s, c);
      }
      paint(c, s);
    });

    // Drop cards no longer present.
    for (const [id, c] of cards) {
      if (!seen.has(id)) { c.root.remove(); cards.delete(id); }
    }
  }

  function paint(c, s) {
    const r = c.refs;
    const meta = PROVIDERS[s.provider] || { icon: "?", c1: "#6ee7ff", c2: "#a78bfa" };
    c.root.style.setProperty("--c1", meta.c1);
    c.root.style.setProperty("--c2", meta.c2);
    r.icon.textContent = meta.icon;
    r.label.textContent = s.label;
    r.kind.textContent = s.kind;
    c.updatedAt = s.updated_at;

    // status state
    let state = "ok";
    if (!s.ok && s.needs_auth) state = "warn";
    else if (!s.ok) state = "error";
    c.root.classList.toggle("state-error", state === "error");
    c.root.classList.toggle("state-warn", state === "warn");
    r.dot.className = "status-dot " + (state === "ok" ? "ok" : state === "warn" ? "warn" : "err");
    r.dot.title = s.ok ? "OK" : s.error || "error";

    // connect button for OAuth providers needing auth
    r.connect.hidden = !s.needs_auth;
    if (s.needs_auth) r.connect.onclick = () => openConnect(s.provider, s.label);

    // --- error / needs-auth presentation ---
    if (!s.ok) {
      r.gaugeWrap.classList.add("no-gauge");
      setGauge(r.fill, null);
      r.bigVal.textContent = s.needs_auth ? "—" : "!";
      r.bigUnit.textContent = "";
      r.metaPrimary.textContent = s.needs_auth ? "Not connected" : "Error";
      r.metaSecondary.textContent = s.error || "";
      r.updated.textContent = s.updated_at ? "checked " + relTime(s.updated_at) : "";
      return;
    }

    // --- value + gauge ---
    const frac = remainingFraction(s);
    r.gaugeWrap.classList.toggle("no-gauge", frac == null);
    setGauge(r.fill, frac);

    let big, unit, fmt;
    if (s.kind === "subscription" && s.unit === "%") {
      big = s.balance != null ? s.balance : 100 - (s.used || 0);
      unit = "% left"; fmt = (n) => Math.round(n);
    } else if (s.kind === "cost") {
      big = s.used || 0; unit = ""; fmt = (n) => fmtMoney(n, s.unit);
    } else {
      big = s.balance != null ? s.balance : (s.used || 0);
      unit = ""; fmt = (n) => fmtMoney(n, s.unit);
    }
    r.bigUnit.textContent = unit;
    countUp(r.bigVal, c.value, big, fmt);
    if (isFinite(c.value) && Math.abs(c.value - big) > 1e-9) {
      r.bigVal.classList.remove("flash"); void r.bigVal.offsetWidth; r.bigVal.classList.add("flash");
    }
    c.value = big;

    // --- meta lines ---
    let p = "", sec = "";
    if (s.kind === "credit") {
      if (s.total != null) { p = "of " + fmtMoney(s.total, s.unit); if (s.used != null) sec = "used " + fmtMoney(s.used, s.unit); }
      else { p = "available balance"; }
    } else if (s.kind === "cost") {
      p = s.window || "spend"; sec = s.extra && s.extra.since ? "since " + s.extra.since : "";
    } else if (s.kind === "subscription") {
      p = s.plan || "subscription";
      if (s.used != null && s.unit === "%") p = (s.plan ? s.plan + " · " : "") + Math.round(s.used) + "% used";
      if (s.reset_at) sec = "resets " + absTime(s.reset_at);
    }
    r.metaPrimary.textContent = p;
    r.metaSecondary.textContent = sec;
    r.updated.textContent = "updated " + relTime(s.updated_at);
  }

  function setGauge(fillEl, frac) {
    if (frac == null) { fillEl.style.strokeDashoffset = GAUGE_CIRC * 0.85; return; }
    fillEl.style.strokeDashoffset = GAUGE_CIRC * (1 - frac);
  }

  // ---------- sparkline ----------
  async function fetchHistory(s, c) {
    try {
      const url = `/api/history?provider=${encodeURIComponent(s.provider)}&label=${encodeURIComponent(s.label)}&hours=24`;
      const res = await fetch(url);
      if (!res.ok) return;
      const { points } = await res.json();
      drawSpark(c.refs, points);
    } catch (_) { /* sparkline is optional */ }
  }

  function drawSpark(r, points) {
    if (!points || points.length < 2) { r.spark.hidden = true; return; }
    const xs = points.map((p) => p.t), ys = points.map((p) => p.v);
    const minX = Math.min(...xs), maxX = Math.max(...xs);
    const minY = Math.min(...ys), maxY = Math.max(...ys);
    const rx = maxX - minX || 1, ry = maxY - minY || 1;
    const pts = points
      .map((p) => `${(((p.t - minX) / rx) * 100).toFixed(2)},${(26 - ((p.v - minY) / ry) * 24).toFixed(2)}`)
      .join(" ");
    r.sparkLine.setAttribute("points", pts);
    r.spark.hidden = false;
  }

  // ---------- summary + countdown ----------
  function updateSummary(list) {
    let total = 0, has = false;
    for (const s of list || []) {
      if (s.ok && s.kind === "credit" && s.balance != null && (s.unit === "USD" || !s.unit)) { total += s.balance; has = true; }
    }
    document.getElementById("total-credit").textContent = has ? fmtMoney(total, "USD") : "—";
  }

  const ringFg = document.getElementById("ring-fg");
  const refreshCount = document.getElementById("refresh-count");
  function tickCountdown() {
    const elapsed = (Date.now() - lastRefresh) / 1000;
    const remain = Math.max(0, pollInterval - elapsed);
    refreshCount.textContent = Math.ceil(remain);
    ringFg.style.strokeDashoffset = -(100 - (remain / pollInterval) * 100);
  }

  function tickRelative() {
    for (const c of cards.values()) {
      const u = c.refs.updated;
      if (u && u.textContent.startsWith("updated") && c.updatedAt) u.textContent = "updated " + relTime(c.updatedAt);
    }
  }

  // ---------- data flow ----------
  async function loadStatus() {
    try {
      const res = await fetch("/api/status");
      const data = await res.json();
      apply(data.monitors || []);
    } catch (e) { setLive(false, "offline"); }
  }

  function apply(list) {
    render(list);
    updateSummary(list);
    lastRefresh = Date.now();
  }

  function setLive(on, text) {
    const dot = document.getElementById("live-dot");
    dot.className = "live-dot " + (on ? "on" : "err");
    document.getElementById("live-text").textContent = text;
  }

  function connectSSE() {
    const es = new EventSource("/api/stream");
    es.addEventListener("status", (ev) => {
      try { apply(JSON.parse(ev.data)); setLive(true, "live"); } catch (_) {}
    });
    es.onopen = () => setLive(true, "live");
    es.onerror = () => { setLive(false, "reconnecting…"); /* EventSource auto-retries */ };
  }

  // ---------- OAuth modal ----------
  const modal = document.getElementById("oauth-modal");
  let oauthCtx = { provider: "", label: "" };

  async function openConnect(provider, label) {
    oauthCtx = { provider, label };
    document.getElementById("oauth-provider").textContent = provider;
    document.getElementById("oauth-msg").textContent = "";
    document.getElementById("oauth-code").value = "";
    const open = document.getElementById("oauth-open");
    open.removeAttribute("href");
    modal.hidden = false;
    try {
      const res = await fetch(`/oauth/start?provider=${encodeURIComponent(provider)}&label=${encodeURIComponent(label)}`);
      const data = await res.json();
      if (data.auth_url) open.href = data.auth_url;
      else setModalMsg(data.error || "could not start OAuth", true);
    } catch (e) { setModalMsg("could not start OAuth", true); }
  }

  function closeModal() { modal.hidden = true; }
  const setModalMsg = (t, err) => {
    const m = document.getElementById("oauth-msg");
    m.textContent = t; m.className = "modal-msg " + (err ? "err" : "ok");
  };

  async function submitOAuth() {
    const code = document.getElementById("oauth-code").value.trim();
    if (!code) return setModalMsg("paste the code first", true);
    setModalMsg("connecting…", false);
    try {
      const res = await fetch("/oauth/complete", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...oauthCtx, code }),
      });
      const data = await res.json();
      if (res.ok && data.ok) {
        setModalMsg("connected ✓", false);
        setTimeout(() => { closeModal(); loadStatus(); }, 900);
      } else {
        setModalMsg(data.error || "connection failed", true);
      }
    } catch (e) { setModalMsg("connection failed", true); }
  }

  document.getElementById("oauth-close").onclick = closeModal;
  document.getElementById("oauth-cancel").onclick = closeModal;
  document.getElementById("oauth-submit").onclick = submitOAuth;
  modal.addEventListener("click", (e) => { if (e.target === modal) closeModal(); });

  // ---------- boot ----------
  loadStatus();
  connectSSE();
  setInterval(tickCountdown, 250);
  setInterval(tickRelative, 1000);
})();
