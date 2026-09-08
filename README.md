# freebuff-proxy

Go wire gateway in front of FreeBuff, with OpenAI-compatible and Anthropic
endpoints plus an embedded Svelte dashboard.

## What it is

- Speaks OpenAI chat (`POST /v1/chat/completions`, `GET /v1/models`) and an
  Anthropic-compatible layer, then translates to the FreeBuff wire protocol.
- Runs in pooled, bridge, or hybrid mode (`EffectiveMode`):
  - **Pooled** — `AUTH_TOKENS` set + `BRIDGE_ENABLED=0`; pool only.
  - **Bridge** — `AUTH_TOKENS` empty; each request carries its own token.
  - **Hybrid** (default with `AUTH_TOKENS`) — `API_KEYS` credential uses the
    pool, any other credential relays upstream as a bridge token.
- Dashboard at `/admin` (Svelte SPA embedded in the binary).
- Freebucks metering follows the wire `prices` map: charged once per session-hour
  at session start, refunded on early `DELETE`, refilled on a Pacific-midnight
  cadence.

## Quickstart

```sh
cp .env.example .env   # then edit: AUTH_TOKENS, ADMIN_TOKEN, ...
go build ./backend/...
go run ./backend/cmd/freebuff-proxy
```

Then:

- `GET http://localhost:3457/healthz` → 200
- `GET http://localhost:3457/v1/models` → live model list
- `http://localhost:3457/admin` → dashboard

Defaults that matter (`.env.example`): `SAFE_MODE=true` (anti-ban preset),
`COST_MODE=free`, 30 req/min and 1500 req/day Pacific limits.

## Layout

- `backend/` — gateway source.
- `frontend/` — dashboard SPA source.
- `scripts/` — upstream sync / drift tooling.
- `docs/` — agent workflow notes.

## Contributing

Protected `main`: branch → PR → green CI → squash merge, Conventional Commits.
See `AGENTS.md` for the full operating guide. Never commit secrets.
