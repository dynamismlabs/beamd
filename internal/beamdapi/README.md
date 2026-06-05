# beamdapi — generated control-plane contract

Go types generated from the **hosted web app's** OpenAPI spec. This is the
shared wire contract between this repo (CLI + edge) and `beamd-web`.

- **Source of truth:** `beamd-web` (Next.js). The spec is built from Zod schemas
  (`@asteasolutions/zod-to-openapi`), served at `GET /api/v1/openapi.json`.
- **This dir:** a committed copy of the spec (`openapi.json`) + generated Go
  types (`types.gen.go`). Don't hand-edit either — edit the schemas in
  `beamd-web` and re-sync.

## What it covers

One spec, three audiences (tagged + per-operation auth):

| Tag | Endpoints | Auth | Consumer |
|---|---|---|---|
| **Device** | `GET /.well-known/beam-auth`, `POST /api/device/code`, `POST /api/device/token` | none | the **CLI** (`internal/devicecode`) |
| **Account** | `GET /api/scopes` | user-session bearer | the **CLI** (`beamd orgs --refresh`) |
| **Internal** | `POST /api/internal/verify-token`, `POST /api/internal/requests`, `GET /api/internal/scope-hostnames`, `GET /api/internal/resolve-host` | shared secret | each **edge** (`internal/auth`, `internal/reqlog`, `internal/edge`) |
| **Workspace / API Keys / Usage** | `GET/POST/DELETE /api/v1/*` | workspace API key | public REST (automation) |

The `DeviceTokenResponse` (`access_token` + `edge` + `scopes`), `VerifyTokenResponse`
(`kind`/`slug`/`scopes`), `RequestEvent`, and `ScopeRef` types here are the same
shapes the hand-written structs in `internal/devicecode/login.go`,
`internal/auth/http_store.go`, and `internal/reqlog` currently model — so you can
migrate those to the generated types to lock alignment.

## Regenerate

```bash
go generate ./internal/beamdapi/...      # writes types.gen.go
```
First run pulls `oapi-codegen` via `go run …@latest` (pin a version in
`generate.go` for reproducibility once you've chosen one).

## Re-sync the spec from beamd-web

The web app owns the spec. When it changes:

```bash
# in beamd-web (dev server running):
pnpm api:export                          # writes openapi/beamd-api.json
cp openapi/beamd-api.json ../beamd/internal/beamdapi/openapi.json
# then here:
go generate ./internal/beamdapi/... && git add internal/beamdapi && git commit
```

(Or in CI, fetch `https://<control-plane>/api/v1/openapi.json` directly.)

## Drift check (CI)

`./check-drift.sh` regenerates and fails if `types.gen.go` is stale — wire it
into CI so the contract can't silently drift. Suggested `Makefile` targets:

```make
api-gen:
	go generate ./internal/beamdapi/...

api-check:
	./internal/beamdapi/check-drift.sh
```
