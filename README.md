# aimon — AI subscription & credit monitor

A small, self-hosted Go web app that watches your **AI credit balances** and
**subscription usage** across providers on one live, dark-themed dashboard.

- 🔄 Polls every provider on an interval (default **60s**) and writes each
  snapshot to **SQLite** — so you get history, not just a current number.
- ⚡ **Live UI** over Server-Sent Events: animated circular gauges, count-up
  numbers, 24-hour sparklines, an aurora backdrop, and a refresh countdown.
- 🔐 Manual **OAuth (PKCE)** connect flow for subscription providers.
- 🐳 Ships as a **~14 MB `scratch` image** — fully static, rootless (uid 65532),
  read-only rootfs, TLS-capable.

---

## Supported providers

| Provider | What it shows | Credential | Status |
|---|---|---|---|
| **DeepSeek** | Prepaid balance (`/user/balance`) | `api_key` | ✅ verified |
| **OpenRouter** | Credit balance + rate-limit (`/credits`, `/key`) | `api_key` | ✅ verified |
| **Anthropic API** | Month-to-date spend (Admin **cost report**) | `admin_key` (`sk-ant-admin-…`) | ✅ verified |
| **Claude Pro/Max** | Subscription connection (usage best-effort) | OAuth (connect in UI) | 🔶 OAuth verified, usage best-effort |
| **x.ai SuperGrok** | Billing-cycle usage (`GetGrokCreditsConfig`) | SuperGrok token via `token_file`/`bearer_token` | ✅ usage / 🔶 api-key = status only |

> **Anthropic** has no public *prepaid balance* API, so aimon reports
> **month-to-date cost** from the Admin cost report instead (needs an
> **Admin** key, not a regular API key).
>
> **Claude** uses the same OAuth client as Claude Code; the token exchange and
> refresh are solid, but the subscription *usage* endpoint is not a stable
> public contract — set `usage_url` if your plan exposes one.
>
> **x.ai SuperGrok** usage comes from the same `grok.com` billing RPC that the
> Grok CLI's `/usage` command reads. Point `token_file` at the credential file
> a SuperGrok OAuth tool keeps refreshed (e.g. `~/.hermes/auth.json` or
> `~/.grok/auth.json`) and aimon reads the live token each poll.

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
    label: "SuperGrok (hermes)"
    token_file: "~/.hermes/auth.json"      # live SuperGrok OAuth token
    # bearer_token: "${XAI_SESSION_TOKEN}" # or a static token
    # api_key: "${XAI_API_KEY}"            # fallback: API-key status only
```

Per-monitor fields: `provider`, `label`, `enabled`, `api_key`, `admin_key`,
`oauth`, `bearer_token`, `token_file`, `usage_url`, `base_url`.

### Connecting Claude (manual OAuth)

1. Click **Connect** on the Claude card.
2. Open the authorization page, approve, and copy the code it shows
   (looks like `code#state`).
3. Paste it back and **Connect**. Tokens are stored in SQLite and
   auto-refreshed.

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
