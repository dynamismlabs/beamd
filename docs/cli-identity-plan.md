# Plan — migrate the CLI to the accounts + scope model

> Build plan for moving the client from the shipped **profiles + `beamd use`**
> model to the **accounts + scope** model in
> [`identity-and-accounts.md`](identity-and-accounts.md). Phased so each phase
> compiles, ships, and never breaks the consumer contract. Checkbox + acceptance
> per item.
>
> **We are the only consumers — no migration shim.** Existing
> `~/.beamd/profiles/*` are not auto-converted; a one-line re-login recreates
> them as accounts. (Per the sole-user / robustness-over-compat stance.)
>
> **Status:** Phases 0, 1, 3 ✅ shipped. **Phase 2 mostly done:** device-code
> login issues a session, a bare `beamd login` targets the baked-in control
> plane (`-X main.DefaultHost`), and the login response assigns the edge +
> caches the scope set (`hosted-mode.md` §2.3). **Remaining:** `orgs --refresh`
> re-fetch, and exercising end-to-end against the live web app.

## Consumer contract — must not break (guardrails for every phase)

The Flow integration and the automation path are frozen:

- `--config <path>` keeps bypassing all identity resolution and reading
  `{server, token, agent_socket, insecure_skip_verify}` verbatim.
- `open` / `close` / `list` / `status` / `check` keep their flags **and** their
  `--json` shapes; `open` still tolerates a not-yet-listening local port.
- The npm bin stays `bin/beamd.cjs`; TLS stays verify-by-default with
  `--insecure` / `insecure_skip_verify` opt-out.
- An OSS `{server, token}` (static token / FileStore) behaves exactly as today:
  no device-code, no orgs, no scope selection.

If a phase would touch any of these shapes, stop and reconsider.

---

## Phase 0 — Rename the store: profiles → accounts (client-only, no protocol change)  `[P0]`

The foundational refactor. Credentials keyed by **server host**, not a profile
name. No edge/proto changes; `--config` untouched.

- [ ] **`internal/config/profiles.go` → `accounts.go`.** Store under
      `~/.beamd/accounts/<sanitized-host>.yaml`; agent socket
      `~/.beamd/agents/<sanitized-host>.sock`. New API:
      `AccountPath(server)`, `AgentSocketFor(server)`, `ListAccounts()`,
      `LoadAccount(server)`, `SaveAccount(*Account)`. Keep `Global` (`current`
      = default account's server + naming `defaults`).
- [ ] **`Account` struct** (extends today's `Client`): `Server`, `Token`,
      `Kind` (`token` | `session`), `InsecureSkipVerify`, `AgentSocket`, and —
      for sessions only — `Scopes []ScopeRef` (cached) + `DefaultScope`.
      OSS accounts set just `{Server, Token}` (+ optional `Slug`).
- [ ] **`cmd/beamd/resolve.go`:** `clientFlags` drops `-p/--profile`, gains
      `--server` + `--scope`. `selectProfile` → `selectAccount` with the ladder
      `--server` → `.beamd server:` → `Global.current` → the only account.
      Add `resolveScope`: `--scope` → `.beamd scope:` → account `DefaultScope`
      → personal (`""`/the account's single slug). `resolveContext` returns
      `{Account, Scope, …}`. **`--config` short-circuit stays byte-identical.**
- [ ] **`.beamd` gains `scope:`** (alongside `server:` / `name:` / `from:`).
      `config.Project` adds `Scope`; parser tolerates it (already
      unknown-key-tolerant).
- [ ] **`cmd/beamd/profile_cmds.go` → `account_cmds.go`:** delete `useCmd` and
      `profilesCmd`. Add:
  - `defaultCmd` — `beamd default [scope]`: show/set the current account's
    `DefaultScope` (personal unless set).
  - `accountsCmd` — list servers logged into, mark `current`. `--json`.
  - `orgsCmd` (alias `scopes`) — list the current account's cached `Scopes`,
    mark default; OSS account → "this server has no org concept". `--json`.
  - `whoamiCmd` — user + server + resolved scope. `--json`.
  - keep `logoutCmd` (now removes an account by `--server`).
- [ ] **`cmd/beamd/main.go`:** dispatch `default` / `accounts` / `orgs` /
      `whoami`; drop `use` / `profiles`; update `usage()`.
- [ ] **Thread server+scope through** `openCmd` / `runCmd` / `closeCmd` /
      `listCmd` / `statusCmd` / `checkCmd` / `reloadCmd` / `mcp` / `agentCmd` —
      they already consume `resolveContext`; mechanical.
- **Acceptance:** `beamd login --server a.test --token T1` and `--server b.test
  --token T2` create two accounts; `beamd open 3000 --server a.test` and
  `--server b.test` both work; `beamd accounts` lists both with `current`
  marked; `beamd default` shows `personal`; `beamd open 3000` (no flags) uses
  `current`; Flow's `--config` path is unaffected (e2e cli tests green).

---

## Phase 1 — Scope in the wire protocol (additive, back-compatible)  `[P0]`

Let one credential authorize many scopes, choosing one per tunnel.

- [ ] **`internal/proto/proto.go`:** `Hello` gains `Scope string
      json:"scope,omitempty"` (requested scope; empty = server default).
      `HelloOK.Slug` is the **resolved** scope (unchanged field).
- [ ] **`internal/client/client.go`:** `Options`/`Connect` accept a requested
      scope; send it in `Hello`; `Slug()` returns the resolved scope from
      `HelloOK`. Empty scope → server picks (personal / the single slug), so
      old behavior is the default.
- [ ] **`internal/auth` `Store`:** lookup takes `(token, requestedScope)` and
      returns the authorized scope (or reject).
  - **HTTPStore** (`http_store.go`): parse the new verify-token response —
    `kind: "session"` → check `requestedScope ∈ scopes` (default to personal
    when empty); `kind: "key"` / bare `{slug}` → the single slug (ignore/return
    the fixed scope). Wire contract per [`hosted-mode.md`](hosted-mode.md) §2.1.
  - **FileStore:** unchanged — one slug; a non-matching requested scope is
    rejected, empty is accepted (flat/assigned), so OSS is untouched.
- [ ] **`internal/edge`:** authorize the requested scope at `hello`; on reject
      send a clear reason (`scope "acme" not available to this login`). Route
      registration uses the resolved scope (existing `naming.Hostname`).
- **Acceptance:** against a fake HTTPStore returning a 2-scope session, a client
  requesting `acme` registers `…acme…`; requesting `nope` is rejected with the
  reason; an OSS FileStore single-slug edge behaves exactly as before; existing
  e2e suite green.

---

## Phase 2 — Interactive login = device-code; org discovery  `[P1]`

- [ ] **`loginCmd`:** if `--token` is passed **or** the server doesn't advertise
      auth discovery → store a static **token** account (today's flow). Else run
      the existing device-code dance (`internal/devicecode`) → store a
      **session** account, cache `Scopes`, set `DefaultScope` (from `--scope`,
      or a pick when multiple, else personal).
- [ ] **Discovery:** probe `/.well-known/beam-auth` (already referenced in
      hosted-mode); present → device-code, absent → token prompt.
- [ ] **`orgs` / `whoami` refresh:** `--refresh` re-fetches the scope set and
      rewrites the account's `Scopes` cache.
- [ ] **`beamd login --scope acme`** sets `DefaultScope` at login (so the
      company-only dev never scopes again).
- **Acceptance:** against the reference web app, `beamd login --server hosted`
  opens the browser flow and writes a session account with cached orgs; `beamd
  orgs` lists them; `beamd open 3000` lands in the default scope; `beamd login
  --server oss --token T` still writes a plain token account.

---

## Phase 3 — User-facing docs + output polish  `[P1]`

Hold these until the commands exist (avoid documenting unshipped behavior).

- [ ] **`README.md`:** replace the "Profiles" section (`beamd login --profile` /
      `beamd profiles` / `beamd use`) with accounts + `beamd default` /
      `beamd orgs` / `beamd accounts`; update the `.beamd` example to show
      `scope:`.
- [ ] **`docs/consuming-beamd.md`:** confirm the `--config` automation path is
      unchanged (it is) and add one line that automation uses a workspace
      **API key**, not a login session.
- [ ] **`docs/setup.md`:** reconcile any `beamd login`/profile references for
      the OSS path (still `--server` + `--token`).
- [ ] **Output:** `open`/`run` print the resolved destination
      (`→ https://3000-acme.beamd.ai`); `status` / `whoami` show account +
      scope; when the account default is used implicitly with >1 account, echo
      it (`primary account; --server / .beamd to change`).
- [ ] **MCP:** add `whoami` (and optionally `scopes`) tool so an agent can see
      where it's pointed; keep `expose_port` returning the URL synchronously.
- **Acceptance:** a fresh reader following README can log in, see their orgs, set
  a default, and open a tunnel; the consumer-contract tests are still green.

---

## Separate track — hosted web app (not the CLI)

The new `verify-token` response (session scope set vs key slug), `teams` /
`memberships` schema, `created_by_user_id`, device-code → **session** issuance,
and the dashboard "Create API key" flow are **web-app** work, specified in
[`hosted-mode.md`](hosted-mode.md) §2–§3, §8. Phase 1's HTTPStore parser is the
only CLI-side dependency on it, and it stays back-compatible with the bare
`{slug}` response — so the CLI phases can land before the web app exists, tested
against a fake store.

## Suggested order & PR boundaries

`Phase 0` (one PR — pure client refactor, big but mechanical) → `Phase 1` (one
PR — proto + auth + edge, additive) → `Phase 2` (login/discovery) → `Phase 3`
(docs/polish). 0 and 1 are independent of the web app; 2 needs the reference app
(or a fake) to exercise device-code end to end.
