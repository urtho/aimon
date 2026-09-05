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
# → http://localhost:8080  ·  sqlite db persists in ./data
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

monitors:
  - provider: deepseek
    label: "DeepSeek"
    api_key: "${DEEPSEEK_API_KEY}"

  - provider: openrouter
    label: "OpenRouter"
    api_key: "${OPENROUTER_API_KEY}"

  - provider: anthropic
    label: "Anthropic API"
    admin_key: "${ANTHROPIC_ADMIN_KEY}"   # sk-ant-admin-...

  - provider: claude
    label: "Claude (Max)"
    oauth: true                            # connect once in the UI

  - provider: xai
    label: "Grok"
    oauth: true                            # or token_file: "~/.grok/auth.json"
    # bearer_token: "${XAI_SESSION_TOKEN}"
    # api_key: "${XAI_API_KEY}"            # fallback: API-key status only
```

Per-monitor fields: `provider`, `label`, `enabled`, `api_key`, `admin_key`,
`oauth`, `bearer_token`, `token_file`, `usage_url`, `base_url`.

### Connecting a subscription (manual OAuth, PKCE)

Both **Claude** and **Grok** use an in-app paste-the-code flow:

1. Click **Connect** on the card.
2. Open the authorization page and approve.
   - **Claude** shows a `code#state` string — copy it.
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
| `GET` | `/api/history?provider=&label=&hours=` | sparkline series |
| `GET` | `/api/stream` | Server-Sent Events (live updates) |
| `GET` | `/oauth/start?provider=&label=` | begin an OAuth flow |
| `POST` | `/oauth/complete` | exchange the pasted code |
| `POST` | `/oauth/disconnect` | forget a stored token |
| `GET` | `/healthz` | liveness |

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
