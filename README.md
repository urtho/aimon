# aimon — AI subscription & credit monitor

A small, self-hosted Go web app that watches your **AI credit balances** and
**subscription usage** across providers on one live, dark-themed dashboard.

- 🔄 Polls every provider on an interval (default **60s**) and writes each
  snapshot to **SQLite** — so you get history, not just a current number.
- ⚡ **Live UI** over Server-Sent Events: plain numbers — USD credits left or
  % quota left, one row per quota window (`92%/5h, resets 4h32m`) — with
  count-up animations, an aurora backdrop, and a refresh countdown.
- 🔐 Manual **OAuth (PKCE)** connect flow for subscription providers.
- 🐳 Ships as a **~14 MB `scratch` image** — fully static, rootless (uid 65532),
  read-only rootfs, TLS-capable.

---

## Supported providers

| Provider | What it shows | Credential | Status |
|---|---|---|---|
| **DeepSeek** | Prepaid balance (`/user/balance`) | `api_key` | ✅ verified |
| **OpenRouter** | Credit balance + rate-limit (`/credits`, `/key`) | `api_key` | ✅ verified |
| **Anthropic API** | Month-to-date spend, or **credits left** (baseline) | `admin_key` (`sk-ant-admin-…`) | ✅ verified |
| **Claude Pro/Max** | Subscription connection (usage best-effort) | OAuth — connect in UI | 🔶 OAuth verified, usage best-effort |
| **OpenAI / Codex** | Subscription quota windows (% left + resets) | OAuth — connect in UI (or `token_file`/`bearer_token`) | 🔶 Internal usage endpoint, best-effort |
| **Grok** | Weekly subscription pool (same meter as Grok Build `/usage`) | OAuth — connect in UI (or `token_file`/`bearer_token`) | ✅ |

> **Anthropic** has no public *prepaid balance* API
> ([open request](https://github.com/anthropics/anthropic-sdk-python/issues/505)),
> so by default aimon reports **month-to-date cost** from the Admin cost report
> (needs an **Admin** key, not a regular API key). To mirror the Console's
> "credits left", set `credit_balance` + `credit_as_of` on the monitor — aimon
> then shows `balance − exact spend since then` (accurate until your next
> top-up, at which point you bump the baseline). Use a **full timestamp** for
> `credit_as_of` (e.g. `2026-06-12T16:00:00+02:00`): the cost report only has
> daily UTC buckets, and a bare date subtracts the whole day's spend — including
> spend from before you read the balance — pushing the value negative. With a
> timestamp, counting starts at the next UTC midnight.
>
> **Claude** uses the same OAuth client as Claude Code; the token exchange and
> refresh are solid, but the subscription *usage* endpoint is not a stable
> public contract — set `usage_url` if your plan exposes one.
>
> **OpenAI** reads the Codex subscription meter through ChatGPT's internal
> `backend-api/wham/usage` endpoint. It shows the returned session, weekly, and
> any additional quota windows, with the tightest window used for history.
> This is Codex subscription usage, not OpenAI API spend or every ChatGPT chat
> model's message cap. OpenAI documents the corresponding fields in the
> [Codex account rate-limit interface](https://learn.chatgpt.com/docs/app-server#6-rate-limits-chatgpt);
> the direct HTTP endpoint is not a stable public API and can change.
> Use **Connect**, a Codex `token_file`, or a ChatGPT OAuth `bearer_token`.
> An OpenAI API key does not provide this meter.
>
> **Grok** usage comes from the same `cli-chat-proxy.grok.com` billing endpoint
> that Grok Build's `/usage` command reads (shared weekly compute pool, shown as
> % used + reset time). Easiest: click **Connect** and authorize via x.ai's
> OAuth (PKCE) — the same public desktop client Grok Build uses; tokens are
> stored and auto-refreshed. Alternatively point `token_file` at
> `~/.grok/auth.json` (kept live by `grok login`), or supply a static
> `bearer_token`. An `XAI_API_KEY` only reports API-key status, not this meter.

---

## Quick start

### Local

```bash
cp config.example.yaml config.yaml      # edit your monitors
export DEEPSEEK_API_KEY=...              # secrets referenced as ${VAR} in config
make run                                 # builds + runs on http://localhost:8080
```

### Docker Compose (bind-mounts the db folder)

```bash
cp config.example.yaml config.yaml
cp .env.example .env                     # put API keys + your UID/GID here
docker compose up -d --build
# → http://localhost:8080  ·  OpenAI OAuth callback on :1455  ·  sqlite db persists in ./data
```

The compose stack runs the container **rootless as your host UID/GID** so the
bind-mounted `./data` is writable, with a **read-only root filesystem** and a
binary self-`healthcheck` (the scratch image has no shell/curl).

---

## Configuration

`config.yaml` is a flat list of monitors. `${VAR}` / `$VAR` are expanded from the
environment at load time, so secrets never need to be committed.

```yaml
listen: ":8080"
database: "./data/aimon.db"      # AIMON_DATABASE env overrides this (compose uses /data)
poll_interval: 60s               # min 10s
# public_url: "http://localhost:8080"   # only needed for browser-redirect OAuth
# oauth_listen: ":1455"                 # Codex/OpenAI loopback; "off" to disable

monitors:
  - provider: claude
    label: "Claude (Max)"
    oauth: true                            # connect once in the UI

  - provider: xai
    label: "Grok"
    oauth: true                            # or token_file: "~/.grok/auth.json"
    # bearer_token: "${XAI_SESSION_TOKEN}"
    # api_key: "${XAI_API_KEY}"            # fallback: API-key status only

  - provider: openai
    label: "OpenAI (Codex)"
    oauth: true                            # or token_file: "~/.codex/auth.json"
    # bearer_token: "${OPENAI_ACCESS_TOKEN}"
    # account_id: "${OPENAI_ACCOUNT_ID}"    # optional workspace override

  - provider: openrouter
    label: "OpenRouter"
    api_key: "${OPENROUTER_API_KEY}"

  - provider: deepseek
    label: "DeepSeek"
    api_key: "${DEEPSEEK_API_KEY}"

  - provider: anthropic
    label: "Anthropic API"
    admin_key: "${ANTHROPIC_ADMIN_KEY}"   # sk-ant-admin-...
    poll_interval: 30m                   # Admin API is rate-limited
```

Cards render in list order. Per-monitor fields: `provider`, `label`, `enabled`,
`poll_interval`, `api_key`, `admin_key`, `oauth`, `bearer_token`, `token_file`,
`account_id`, `usage_url`, `authorize_url`, `token_url`, `base_url`.

For OpenAI, credentials are selected in this order: `token_file`, `bearer_token`,
then stored in-app OAuth. The account/workspace is read from the credential file
or token claims; `account_id` overrides it. With `base_url`, use a ChatGPT backend
base (normally `https://chatgpt.com/backend-api`); `usage_url` overrides the full URL.

To reuse a Codex login, set `token_file: "~/.codex/auth.json"` (or the `auth.json`
under your custom `CODEX_HOME`). It must contain ChatGPT login tokens; API-key
login and credentials stored only in the OS keyring cannot be read this way.
aimon rereads the file on every poll, without writing it or rotating its tokens.
Keep Codex signed in so it refreshes the file; if authorization fails, sign in
again with Codex. A static `bearer_token` must be replaced when it expires.

For Docker, **Connect** needs no extra mounts. Compose publishes host port
**1455** so the OpenAI/Codex browser redirect can reach the container. To use a
token file instead, mount the host Codex directory read-only, for example
`/home/you/.codex:/codex:ro`, and configure `token_file: "/codex/auth.json"`.
Mounting the directory lets aimon see credentials replaced atomically by Codex.
The container user must have read access to the file.

### Connecting a subscription (OAuth, PKCE)

**OpenAI / Codex** uses a loopback redirect that aimon serves on port **1455**:

1. Click **Connect** on the card and open the authorization page.
2. Sign in with ChatGPT. The browser returns to
   `http://localhost:1455/auth/callback`, aimon stores the tokens, and the
   dashboard updates. You can close the success tab.
3. If port 1455 is blocked or something else is bound there, paste the full
   callback URL from the address bar into the Connect dialog instead.

**Claude** and **Grok** still use the paste-the-code flow:

1. Click **Connect** on the card.
2. Open the authorization page and approve.
   - **Claude** shows a `code#state` string — copy it.
     For a second Claude account (e.g. a Team seat), open the authorization
     page in a private window (or sign claude.ai out first) so you approve
     with that account, not your default login.
   - **x.ai** redirects to a loopback URL (`127.0.0.1:56121/…`) that won't load;
     copy the **full URL from your browser's address bar** instead.
3. Paste it back and **Connect**. Tokens are stored in SQLite and auto-refreshed.

---

## How it works

```
config.yaml ─▶ providers ─▶ poller (every 60s, concurrent)
                                │
                  ┌─────────────┼───────────────┐
                  ▼             ▼                ▼
              SQLite        in-memory        SSE hub ──▶ browser (live)
            (snapshots)   latest cache
```

- `internal/providers` — one fetcher per service → a normalized `model.Status`.
- `internal/poller` — concurrent refresh loop, persists every snapshot, fans
  results out to SSE subscribers, prunes data older than 30 days.
- `internal/store` — pure-Go SQLite (`modernc.org/sqlite`, no cgo).
- `internal/oauth` — OAuth2 authorization-code + PKCE with the paste-the-code
  flow and token refresh.
- `internal/server` + `web/` — embedded dashboard, JSON/SSE API, OAuth routes.

### HTTP endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | dashboard |
| `GET` | `/api/status` | latest status of every monitor (JSON) |
| `GET` | `/api/waybar` | Waybar custom-module JSON (`text`, `tooltip`, `class`, `percentage`) |
| `GET` | `/api/history?provider=&label=&hours=` | sparkline series |
| `GET` | `/api/stream` | Server-Sent Events (live updates) |
| `GET` | `/oauth/start?provider=&label=` | begin an OAuth flow |
| `POST` | `/oauth/complete` | exchange the pasted code |
| `POST` | `/oauth/disconnect` | forget a stored token |
| `GET` | `/healthz` | liveness |
| `GET` | `http://localhost:1455/auth/callback` | OpenAI/Codex OAuth loopback (separate listener) |

`/api/waybar` is the latest snapshot already formatted for a [Waybar](https://github.com/Alexays/Waybar) `custom` module (`return-type: json`). Optional query filters (`provider`, `label`, `kind`) take comma-separated, case-insensitive values — e.g. `?kind=subscription` or `?provider=claude,xai,openai`.

```json
{
  "text": "C 92% · X 58% · AI 75%",
  "tooltip": "Claude (Max)  Max\n  80%/1w  resets 4d2h  below · linear 58% left\n…",
  "class": "over",
  "alt": "over",
  "percentage": 58,
  "monitors": [{ "provider": "claude", "text": "C 92%", "class": "below", "pace": "below", "percentage": 92 }]
}
```

Subscription bar values (%, color, tooltip) use **weekly** quota windows; a monitor with no weekly window (e.g. a Claude Team seat that only reports a 5h session cap) falls back to its session window. Subscription colors compare **used %** to a **linear burn** across each quota window (elapsed / duration). Within ±5 percentage points is `at`; slower than that is `below` (under budget); faster is `over`. `class` / `alt` is the worst state among the (filtered) monitors: `below`, `at`, `over`, `ok` (credits, or no window), `auth` (needs Connect), `error`, or `empty`. `percentage` is the tightest remaining quota among healthy monitors. Compact tokens use `C` / `X` / `AI` / `OR` / `DS` / `AN`; `text` is Pango-colored per monitor (set Waybar `markup` to `pango`). When a provider has several monitors (e.g. a personal Claude Max plus a Team seat), the first one in config order keeps the plain token and later ones are numbered — `C`, `C2`, … — even when the gadget filters down to one label (`?label=Claude%20(Team)`).

### Waybar module

aimon must be reachable from the bar host (typically `http://127.0.0.1:8080`), and `curl` must be on `PATH`. Connect OAuth providers in the dashboard first or the bar will show `C ?` / `AI ?`. Poll no faster than `poll_interval` (default 60s).

In `~/.config/waybar/config` (or `config.jsonc`), add `"custom/aimon"` to a `modules-*` list and:

```jsonc
"custom/aimon": {
    "exec": "curl -fsS --max-time 2 http://127.0.0.1:8080/api/waybar?kind=subscription || echo '{\"text\":\"aimon !\",\"tooltip\":\"aimon unreachable\",\"class\":\"error\",\"alt\":\"error\"}'",
    "return-type": "json",
    "markup": "pango",
    "interval": 60,
    "tooltip": true,
    "on-click": "xdg-open http://127.0.0.1:8080"
}
```

One gadget per provider: `?provider=claude`, `?provider=xai`, `?provider=openai`. In `~/.config/waybar/style.css`:

```css
#custom-aimon.below { color: #7dffb3; }
#custom-aimon.at { color: #ffcc66; }
#custom-aimon.over { color: #ff5555; }
#custom-aimon.warn,
#custom-aimon.auth { color: #ffcc66; }
#custom-aimon.critical,
#custom-aimon.error { color: #ff5555; }
#custom-aimon.empty { color: #888; }
```

Then `killall -SIGUSR2 waybar` (or restart Waybar) so it reloads the config.

---

## Development

```bash
make help     # list targets
make dev      # run with debug logging
make test     # tests across (nproc-1) cores
make image    # build the scratch image locally
make docker   # buildx build + push with SBOM + provenance attestations
```

Secrets (`config.yaml`, `.env`, `./data/*.db`) are gitignored.

## License

MIT
