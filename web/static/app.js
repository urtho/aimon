/* aimon dashboard — plain-number cards, span rows, SSE, OAuth modal */
(() => {
  "use strict";

  // Per-provider visual identity.
  const PROVIDERS = {
    deepseek:   { icon: "DS", c1: "#5b76ff", c2: "#8b5bff" },
    openrouter: { icon: "OR", c1: "#22d3ee", c2: "#3b82f6" },
    anthropic:  { icon: "AN", c1: "#d97757", c2: "#f59e0b" },
    claude:     { icon: "C",  c1: "#d97757", c2: "#c0623f" },
    openai:     { icon: "AI", c1: "#10a37f", c2: "#34d399" },
    xai:        { icon: "𝕏",  c1: "#cbd5e1", c2: "#7c8aa3" },
  };

  const cardsEl = document.getElementById("cards");
  const emptyEl = document.getElementById("empty");
  const tpl = document.getElementById("card-tpl");

  const cards = new Map(); // id -> { root, refs, value, updatedAt, spans }
  let oauthCtx = { provider: "", label: "", state: "" };
  const setModalMsg = (t, err) => {
    const m = document.getElementById("oauth-msg");
    m.textContent = t; m.className = "modal-msg " + (err ? "err" : "ok");
  };

  // ---------- formatting ----------
  const fmtMoney = (n, unit) => {
    const sym = { USD: "$", CNY: "¥", EUR: "€", GBP: "£" }[unit];
    const v = Math.abs(n) >= 1000 ? n.toFixed(0) : n.toFixed(2);
    if (sym) return sym + Number(v).toLocaleString();
    return Number(v).toLocaleString() + (unit ? " " + unit : "");
  };

  const relTime = (sec) => {
    if (!sec) return "—";
    const d = Math.max(0, Math.floor(Date.now() / 1000 - sec));
    if (d < 5) return "just now";
    if (d < 60) return d + "s ago";
    if (d < 3600) return Math.floor(d / 60) + "m ago";
    if (d < 86400) return Math.floor(d / 3600) + "h ago";
    return Math.floor(d / 86400) + "d ago";
  };

  // Compact duration until a reset: 4d2h, 4h32m, 32m.
  const fmtDur = (sec) => {
    if (sec <= 0) return "now";
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + "d" + (h > 0 ? h + "h" : "");
    if (h > 0) return h + "h" + (m > 0 ? String(m).padStart(2, "0") + "m" : "");
    return Math.max(1, m) + "m";
  };

  // ---------- card lifecycle ----------
  function makeCard(id) {
    const root = tpl.content.firstElementChild.cloneNode(true);
    const refs = {
      root,
      icon: root.querySelector(".prov-icon"),
      label: root.querySelector(".label"),
      kind: root.querySelector(".kind"),
      dot: root.querySelector(".status-dot"),
      valueWrap: root.querySelector(".value-wrap"),
      bigVal: root.querySelector(".big-value"),
      bigUnit: root.querySelector(".big-unit"),
      spans: root.querySelector(".spans"),
      metaPrimary: root.querySelector(".meta-line.primary"),
      metaSecondary: root.querySelector(".meta-line.secondary"),
      updated: root.querySelector(".updated"),
      connect: root.querySelector(".btn-connect"),
    };
    cards.set(id, { root, refs, value: NaN, updatedAt: 0, spans: null });
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

    list.forEach((s) => {
      const id = s.provider + "/" + s.label;
      seen.add(id);
      let c = cards.get(id);
      if (!c) {
        c = makeCard(id);
      }
      cardsEl.appendChild(c.root);
      paint(c, s);
    });

    // Drop cards no longer present.
    for (const [id, c] of cards) {
      if (!seen.has(id)) { c.root.remove(); cards.delete(id); }
    }
  }

  // One row per quota span: "92%  /5h        resets 4h32m"
  function renderSpans(r, spans) {
    if (!spans || !spans.length) {
      r.spans.hidden = true;
      r.spans.replaceChildren();
      return;
    }
    r.spans.hidden = false;
    r.spans.replaceChildren(...spans.map((sp) => {
      const left = Math.max(0, Math.round(100 - sp.used));
      const row = document.createElement("div");
      row.className = "span-row";

      const pct = document.createElement("span");
      pct.className = "span-pct" + (left <= 10 ? " low" : left <= 25 ? " mid" : "");
      pct.textContent = left + "%";

      const name = document.createElement("span");
      name.className = "span-name";
      name.textContent = "/" + sp.name;

      const reset = document.createElement("span");
      reset.className = "span-reset";
      reset.textContent = sp.reset_at ? "resets " + fmtDur(sp.reset_at - Date.now() / 1000) : "";

      row.append(pct, name, reset);
      return row;
    }));
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
    c.spans = null;

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

    r.updated.textContent = s.updated_at
      ? (s.ok ? "updated " : "checked ") + relTime(s.updated_at)
      : "";

    // --- error / needs-auth ---
    if (!s.ok) {
      r.valueWrap.hidden = false;
      renderSpans(r, null);
      r.bigVal.textContent = s.needs_auth ? "—" : "!";
      r.bigUnit.textContent = "";
      r.metaPrimary.textContent = s.needs_auth ? "Not connected" : "Error";
      r.metaSecondary.textContent = s.error || "";
      c.value = NaN;
      return;
    }

    if (s.kind === "subscription") {
      if (s.spans && s.spans.length) {
        // One row per quota window; no big number.
        r.valueWrap.hidden = true;
        c.spans = s.spans;
        renderSpans(r, s.spans);
        r.metaPrimary.textContent = s.plan || "";
        r.metaSecondary.textContent = "";
      } else {
        r.valueWrap.hidden = false;
        renderSpans(r, null);
        if (s.balance != null) {
          r.bigVal.textContent = Math.round(s.balance) + "%";
          r.bigUnit.textContent = "quota left";
        } else {
          r.bigVal.textContent = "✓";
          r.bigUnit.textContent = "connected";
        }
        r.metaPrimary.textContent = s.plan || "";
        r.metaSecondary.textContent = s.extra && s.extra.usage === "unavailable" ? "usage n/a" : "";
      }
      c.value = NaN;
      return;
    }

    // --- credit / cost: one big number ---
    r.valueWrap.hidden = false;
    renderSpans(r, null);

    let big, unit;
    if (s.kind === "cost") {
      big = s.used || 0;
      unit = "spent";
    } else { // credit
      big = s.balance != null ? s.balance : (s.used || 0);
      unit = "left";
    }
    r.bigUnit.textContent = unit;
    r.bigVal.textContent = fmtMoney(big, s.unit);
    c.value = big;

    let p = "", sec = "";
    if (s.kind === "credit") {
      if (s.total != null) {
        p = "of " + fmtMoney(s.total, s.unit);
        if (s.used != null) sec = "used " + fmtMoney(s.used, s.unit);
      } else {
        p = "available balance";
      }
    } else {
      p = s.window || "spend";
      sec = s.extra && s.extra.since ? "since " + s.extra.since : "";
    }
    r.metaPrimary.textContent = p;
    r.metaSecondary.textContent = sec;
  }

  // ---------- summary + tickers ----------
  function updateSummary(list) {
    let total = 0, has = false;
    for (const s of list || []) {
      if (s.ok && s.kind === "credit" && s.balance != null && (s.unit === "USD" || !s.unit)) { total += s.balance; has = true; }
    }
    document.getElementById("total-credit").textContent = has ? fmtMoney(total, "USD") : "—";
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
    const oauthModal = document.getElementById("oauth-modal");
    if (oauthModal && !oauthModal.hidden && oauthCtx.provider) {
      const match = (list || []).find((s) => s.provider === oauthCtx.provider && s.label === oauthCtx.label);
      if (match && match.ok && !match.needs_auth) {
        setModalMsg("connected ✓", false);
        setTimeout(() => { oauthModal.hidden = true; }, 800);
      }
    }
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

  const PROV_NAMES = { xai: "Grok", claude: "Claude", openai: "OpenAI / Codex" };
  const STEP2 = {
    openai: "After you approve, a success page should open on localhost:1455 — close it; this dashboard updates automatically. If that page cannot load, paste the full callback URL from the address bar.",
    xai: "x.ai redirects to a loopback URL that will not load. Copy the full URL from the address bar.",
    claude: "Copy the code#state string shown on the page.",
  };

  async function openConnect(provider, label) {
    oauthCtx = { provider, label, state: "" };
    document.getElementById("oauth-provider").textContent = PROV_NAMES[provider] || provider;
    document.getElementById("oauth-msg").textContent = "";
    document.getElementById("oauth-code").value = "";
    const step2 = document.getElementById("oauth-step-2");
    if (step2) step2.textContent = STEP2[provider] || "Copy the code it shows, or the full URL from the address bar.";
    const open = document.getElementById("oauth-open");
    open.removeAttribute("href");
    modal.hidden = false;
    try {
      const res = await fetch(`/oauth/start?provider=${encodeURIComponent(provider)}&label=${encodeURIComponent(label)}`);
      const data = await res.json();
      if (data.auth_url) {
        open.href = data.auth_url;
        // Remember the state we started with — the pasted code may not carry it.
        oauthCtx.state = data.state || "";
      } else {
        setModalMsg(data.error || "could not start OAuth", true);
      }
    } catch (e) { setModalMsg("could not start OAuth", true); }
  }

  function closeModal() { modal.hidden = true; }

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
})();
