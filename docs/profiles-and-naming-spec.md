# Spec — Profiles, project context, and tunnel naming

> Self-contained build plan. Each item is a checkbox with concrete
> acceptance. You shouldn't need anything beyond this file + the codebase.

> ⚠️ **Identity half superseded.** §1 (Profiles), the "many identities" framing
> in §0, and every `beamd use` / `beamd profiles` reference below describe the
> *original* per-edge-**profile** design — which **shipped**, but is now
> replaced by the accounts + scope model in
> [`identity-and-accounts.md`](identity-and-accounts.md): credentials are keyed
> per **server** (an *account*, not a "profile"), orgs are a **scope selector**
> (`--scope` / `beamd.yaml` / `beamd default`), and there is **no `beamd use`**.
> The **naming half is current and accurate** — §2 (`--as`/`--from`), §3
> (`beamd.yaml`), Convergence, and the `run` framework-reachability work all stand
> as shipped. The CLI migration from profiles→accounts is tracked in
> [`cli-identity-plan.md`](cli-identity-plan.md).

## 0. Background & goal

beamd's client today logs into **one** edge (`~/.beamd/config` = a single
`{server, token}`), and tunnel names are the port number unless you pass
`--as`. Two problems:

1. **Login churn.** Working across companies/projects means logging out and
   back in to switch edges. We want to be logged into **many at once** and
   select seamlessly — the kubectl/`gh auth`/AWS-profile model.
2. **Naming is rigid.** People want the label to come from the port, the
   folder, the git repo, or the branch — and have project-level defaults.

**Unifying idea.** Every `beamd open` (and `beamd run`, which wraps it)
resolves two contexts — *which edge* (→ `<slug>.<base>`) and *what name*
(→ `<label>`). They're the same shape of problem, so they share **one
precedence ladder** and one project file — and both commands consume the
result through the same code path (see "Convergence" below).

**Decisions locked (apply throughout):**
- **One ladder** (highest wins): `CLI flag` → `env` → project `beamd.yaml`
  (nearest, walking up from cwd) → global config → built-in default.
- **Secrets are global-only.** A `beamd.yaml` file references a profile/server;
  it never contains a token. That's what makes it safe to share.
- **No naming DSL.** Derivation is a small fixed menu on a `--from` flag
  (`--from dir`); an explicit literal is `--as`. No `@token` template —
  composition (e.g. `myapp-api`) is just a literal `--as` value.
- **Default name stays `port`.** Nicer naming is opt-in, so no URL changes
  out from under anyone.
- **The programmatic path is unaffected.** Automation (Flow) passes
  `--config <path>` + `--as <name>` explicitly and bypasses all of this;
  profiles / `beamd.yaml` / strategies are human-CLI ergonomics. (`--config`
  takes precedence over `--profile`.)

**Suggested order:** §1 → §2 → §3 → Convergence (incl. the
framework-reachability work) → §4 → §5. The edge-hardening appendix is a
**separate track** (it touches `serve`, not the client) and can land anytime.

---

## 1. Profiles — many identities, zero logout churn  `[P0 — the pain-killer]`

Be logged into every edge at once; switch with a flag, env, or a default.

**Storage** (reuses the existing client-config loader):
- `~/.beamd/profiles/<name>` — one file per profile, the existing client
  config YAML (`server`, `token`, …).
- `~/.beamd/config` — top-level: `current: <name>` + global defaults
  (e.g. a default `from:` / `name:`).
- Per-profile agent socket lives at `~/.beamd/agents/<name>.sock` (used by §5).
- **Backward compat:** a legacy `~/.beamd/config` containing `server`/`token`
  is migrated to profile `default` (and `current: default`) on first run —
  existing users keep working unchanged.

**Tasks**
- [ ] Profile store: load/save `profiles/<name>`, read/write the `current`
      pointer, and the one-time legacy-config → `default` migration.
- [ ] Profile resolution helper: `--profile/-p <name>` → `BEAMD_PROFILE`
      env → `beamd.yaml` `profile:` (§3) → `current`. An explicit `--config
      <path>` short-circuits this (loads that file directly).
- [ ] Add `-p/--profile` to every client command: `login`, `open`, `close`,
      `list`, `status`, `run`, `mcp`.
- [ ] `beamd login [--profile <name>]` creates/updates a profile **without
      clobbering others**; the first profile created becomes `current`.
      Reuse the interactive prompts + `:443` default already built.
- [ ] `beamd use <name>` sets `current`.
- [ ] `beamd profiles` lists each profile: name, server, slug, and a marker
      on `current`. `--json` returns the same.
- [ ] `beamd logout [--profile <name>]` removes a profile file (default:
      `current`); if it was `current`, repoint to another or clear it.
- **Acceptance:** log into two edges as profiles `a` and `b`; `beamd open
  3000 -p a` and `beamd open 3000 -p b` both work **with no logout in
  between**; `beamd profiles` shows both with `current` marked; `beamd use
  b` changes the default so a bare `beamd open 3000` uses `b`;
  `BEAMD_PROFILE=a beamd status` reports profile `a`.

---

## 2. Tunnel naming — `--as` (literal) + `--from` (derive), no DSL  `[P1]`

Precedence: `--as` / `--from` → project `beamd.yaml` → global default → `port`.

- **`--as <label>`** — an explicit literal label (e.g. `web-api`).
- **`--from <source>`** — derive the label from a fixed menu:
  - `port` — the port number (the built-in default)
  - `dir` — basename of cwd (covers pwd *and* worktree dirs)
  - `repo` — git repo name (`basename` of `git rev-parse --show-toplevel`)
  - `branch` — current git branch, sanitized (`feat/x` → `feat-x`)

Two crisp, **unambiguous** flags: `--from repo` always derives, `--as repo`
always means the literal "repo". `--from` is short enough to type;
`--help` / `beamd init` document the menu. If both are given, `--as`
(explicit) wins. Config keys mirror them — `name:` (literal) or `from:`
(source) — in both `beamd.yaml` and the global config.

**Tasks**
- [ ] `--as` and `--from` on `open` and `run` (mutually exclusive; `--as` wins).
- [ ] Derivation functions for `dir` / `repo` / `branch` (+ `port`), each
      run from the invocation's cwd, then **sanitized to a single valid
      RFC 1123 label** (lowercase, alnum+hyphen, ≤63, collapse/strip
      invalid). Reuse `naming.ValidateLabel`; if a source can't be made
      valid (e.g. detached HEAD for `branch`, not in a git repo for
      `repo`), fail with a clear, actionable error.
  - **Truncate with a hash suffix, not a hard cut.** When a derived label
    exceeds 63 chars, drop to `name[:56] + "-" + sha256(name)[:6]` so two
    long-but-distinct names don't collide on the same truncation. (Pattern
    from portless `auto.ts` `truncateLabel`.)
  - **`repo` strips the npm scope** when sourced from a package name
    (`@org/app` → `app`), matching portless `inferProjectName`.
  - **Worktree caveat — beamd is one DNS label deep.** Portless gives linked
    worktrees a *subdomain* prefix (`feat-x.myapp.localhost`). beamd's
    wildcard is `*.<slug>.<base>` — a single label — so a worktree can't add
    a DNS level. If we want the same ergonomic, compose into **one** label
    (`feat-x-myapp`), and only when in a *linked* worktree (detect via `git
    rev-parse --git-dir` ≠ `--git-common-dir`, skip `main`/`master`) — never
    in the primary checkout. Treat this as an optional `--from worktree`
    source, not a change to `dir`/`repo`/`branch`.
- [ ] Name resolution helper wired into `open`/`run` following the ladder
      (`--as`/`--from` → `beamd.yaml` `name:`/`from:` → global → `port`).
- **Acceptance:** in `~/work/myapp`, `beamd open 3000 --from dir` →
  `myapp.<slug>.<base>`; `--from branch` on `feat/x` → `feat-x.…`; `--from
  repo` outside a git repo errors clearly; with nothing set the label is
  the port; `--as web-api` yields exactly `web-api`.

---

## 3. Project context `beamd.yaml`  `[P1 personal · P2 team]`

A per-project file (YAML, **never a secret**) supplying the identity and/or
naming default, found by **walking up from cwd** to the first `beamd.yaml`
(stop at `$HOME` or the filesystem root). Two ways to point at an edge:

```yaml
# personal beamd.yaml — gitignore it, like .env
profile: acme             # references a global profile by name
from: repo                # a derive source, or `name: <literal>` (see §2)
```
```yaml
# shared/committed beamd.yaml — references the edge canonically
server: tunnel.acme.com   # globally unique; matched against your profiles
from: repo
```

- [ ] **(P1)** Discover `beamd.yaml` by walking up from cwd; parse
      `profile`/`server` and `name`. `profile:` → that named profile; if it
      doesn't exist locally, error "this project uses profile `acme` — run
      `beamd login --profile acme`". Personal/gitignored convention.
- [ ] **(P2)** `server:` matching — resolve to the local profile whose
      server matches, so a *committed* file works for any teammate
      regardless of what they named their profile. If none matches, guide
      login: "this project tunnels through `tunnel.acme.com` — run `beamd
      login`" (offer the browser/device-code flow when the edge advertises
      it).
- [ ] **(P2) First-use trust.** *(DEFERRED — hosted-era; interactive y/N +
      `trusted_servers` in the global config. Server-matching that this gates
      is implemented; the prompt is not.)* The first time a `beamd.yaml` points your client
      at a **new** server, confirm before connecting ("This project wants to
      tunnel through `tunnel.acme.com` — allow? [y/N]") and remember the
      answer. A committed file silently redirecting your local ports to an
      arbitrary edge is a real risk; one y/n closes it without nagging.
- **Acceptance (P1):** a personal `beamd.yaml` `{profile: acme, from: repo}` →
  bare `beamd open 3000` anywhere in the tree uses `acme` + the repo name;
  flags override; outside the tree it's `current` + `port`.
- **Acceptance (P2):** a committed `beamd.yaml` `{server: tunnel.acme.com, from:
  repo}` → a teammate already logged into that edge (under any profile name)
  gets the right edge + repo-named tunnel after one trust prompt; someone
  not logged in is guided to log in.

### Why a shared `beamd.yaml` is a growth lever (and how to earn it)

A committed `beamd.yaml` carries the team's institutional knowledge — *which
edge, named how* — so a new dev does `git clone` → `beamd open` (or `npm run
dev` wired to `beamd run`) and gets a working preview URL with **near-zero
setup**. That's the spread mechanic: one person adds it, the whole team
inherits it (the way `.nvmrc` / prettier configs propagate), and previews
become a team norm → more usage → more value.

Two things make it actually frictionless, and they're why this is a **P2 /
hosted-era** play, not P1:
- **Auth has to be easy.** Committed config still leaves "get a credential
  for that edge." With self-hosted static tokens that's a manual hand-off;
  the lever only fully unlocks with **device-code / SSO login** (the
  deferred hosted feature) — clone → `beamd login` (browser) → go. So design
  `beamd.yaml`'s `server:` form now (above) to keep that door open.
- **Trust has to be explicit** (the first-use confirmation above) — otherwise
  a shared config is a vector for redirecting someone's traffic.

For the hosted product this doubles as the funnel: a committed `beamd.yaml` +
device-code login means each teammate who clones becomes a signed-in,
attributable user on the org's account.

---

## Convergence — how `open` / `run` consume it  `[P0/P1, no new code path]`

`open` and `run` resolve the **same** context — `{profile (server+token),
label}` — via the one ladder above, and consume it through the **shared
`dialAndRegister` path** (already factored out, so there's nothing to
duplicate). `run` is just `open` wrapped around a child process; everything
in §1–§3 applies to it identically:

- [ ] `beamd run` resolves profile (§1) and name (§2/§3) exactly like
      `open` — same resolver, same `dialAndRegister`, no separate path.
- [ ] Let `beamd run` **omit the `<name>` positional** and derive the label
      from the ladder (`--as`/`--from` → `beamd.yaml` → default). Today `run`
      requires a name; after this, `beamd run -- npm run dev` works whenever
      a `beamd.yaml` or `--from` supplies the name.

This is the **payoff the whole spec builds toward** — the seamless
end-state lives in `run`:

```
# package.json:  "dev:tunnel": "beamd run -- npm run dev"
# repo has a committed beamd.yaml { server: tunnel.acme.com, from: repo }

$ npm run dev:tunnel
→ right account (acme) + right name (the repo) + a working preview URL,
  zero flags, cleans up on exit.
```

- **Acceptance:** with a `beamd.yaml` supplying profile + `from`, a bare `beamd
  run -- <cmd>` (no name, no flags) brings the command up on the right edge
  with the right label; `-p`/`--as`/`--from` override exactly as for `open`.

### `run` makes any framework reachable  `[P1 — the portless lessons]`

`run` resolving the right edge + name (above) is wasted if the wrapped dev
server then **rejects the tunnel's Host** or **never binds the port we
expose**. Today `runCmd` only sets `PORT=<n>` and `exec.Command`s directly —
which silently fails for a whole class of apps. These tasks make `beamd run
-- <framework>` actually work, end to end. (Derived from a full read of
portless; see `ignore/portless-deep-dive.md` §5.)

- [ ] **Allowed-hosts injection (the must-fix).** A tunnel surfaces a remote
      Host (`<label>.<slug>.<base>`); Vite answers `Blocked request. This
      host is not allowed.` and Next dev warns on cross-origin. Inject the
      env that opens this up, keyed to the resolved tunnel domain:
      `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS=.<slug>.<base>` for Vite. Next's
      `allowedDevOrigins` is config-only (no env) — detect a Next project and
      **print a one-line hint** with the exact value to add. This bites beamd
      harder than portless (portless's child is reached at localhost; ours is
      a fully remote host), so it's the single highest-value item here.
- [ ] **Framework flag table for the `PORT`-ignorers.** Most apps honor
      `PORT` (we already set it). The holdouts —
      `vite`, `vp`, `react-router`, `rsbuild`, `astro`, `ng`,
      `react-native`, `expo` — ignore it. For those, append `--port <n>`
      (+ `--strictPort` for the Vite family so it *hard-fails* instead of
      drifting to a port we're not tunneling) and `--host 127.0.0.1`.
      **Runner-aware**: look past `npx`/`bunx`/`pnpx`/`yarn dlx`/`pnpm exec`
      and their flags to find the real binary. **Idempotent**: skip if the
      user already passed `--port`/`--host`. (Expo's `--host` is a mode, not a
      bind addr — special-case or just inject `--port`.)
- [ ] **Resolve project-local binaries.** Spawn through a shell with
      `./node_modules/.bin` prepended to `PATH` (and node's own dir), so
      `beamd run -- vite` finds the project's vite instead of erroring. Keep
      spawning in the child's own process group so Ctrl-C tears down the whole
      tree (already done).
- [ ] **Inject `BEAMD_URL`** (the public `https://<label>.<slug>.<base>`) into
      the child, so apps can self-reference for OAuth callbacks, absolute
      links, and webhooks — the direct fix for the "I hardcoded localhost"
      class of bug.
- [ ] **Fail fast, before spawning.** Validate the resolved profile (auth
      present) and dial the edge *before* launching the wrapped command, so a
      bad profile / unreachable edge errors immediately instead of starting
      the dev server and then 502-ing. (Pattern from portless's
      `ensureTailscaleReady`, which validates everything before touching the
      child.) Keep `waitListening` — advertising a URL only once the child is
      actually up is better than portless's serve-502-until-ready.
- **Acceptance:** `beamd run web --from dir -- npm run dev` against a Vite app
  serves through `https://web.<slug>.<base>` with **no** "host not allowed"
  error and the page live; `beamd run -- vite` (project-local, no global vite)
  resolves the binary; the child sees `BEAMD_URL`; an invalid `-p bad`
  errors before the dev server starts.

## 4. Guided setup `beamd init`  `[P2]`  — DEFERRED

> **DEFERRED: `beamd init` already exists** as the *edge* setup command
> (writes `beamd.yaml`, used in docs/setup.md). The client `init` here (write
> `beamd.yaml`) collides with it. Resolve the verb before building — e.g. client
> project setup under a different name (`beamd setup` / `beamd project init`),
> or move edge init under `beamd serve init`. Not built to avoid breaking the
> documented edge command.

So the first run is taught, not looked up.

- [ ] `beamd init` (interactive, in a project dir): pick a profile (from
      existing, or "log in to a new one" → runs §1 login), pick a naming
      source (`port`/`dir`/`repo`/`branch`/custom literal), then write
      `beamd.yaml`. Reuse the prompt UX.
- [ ] Non-interactive: `beamd init --profile <name> [--name-from <src> |
      --as <literal>]`.
- **Acceptance:** `beamd init` in a fresh project writes a valid `beamd.yaml`;
  a subsequent bare `beamd open 3000` honors it.

---

## 5. Detached tunnels per profile  `[P2]`

Foreground tunnels are already multi-profile (each `open` connects with its
resolved profile). Detached needs per-profile agents.

- [ ] Key the agent socket by profile (`~/.beamd/agents/<name>.sock`) so each
      profile's detached tunnels are held by its own agent; `ensureAgent`
      spawns/targets the selected profile's socket.
- [ ] `list` / `close` / `status` operate on the **selected profile's** agent
      (and never spawn one — unchanged semantics).
- **Acceptance:** `beamd open 3000 -p a -d` and `beamd open 3001 -p b -d`
  run concurrently; `beamd list -p a` shows only `a`'s tunnel; closing one
  doesn't touch the other.

---

## Evolving personal → shared (build P1 so P2 is additive)

The expansion is cheap *only if* P1 resolves to abstractions, not one-offs.
Build these seams in P1 even though only the personal path uses them:

- [ ] **Resolve to identity, not a name.** One `resolveContext()` returns the
      concrete `{server, token}` + label; every command consumes *that*,
      never a profile *name*. (P2's `server:` lookup has no name — so the
      name must not be the currency anywhere downstream.)
- [ ] **Profiles store their `server`** (they already do) → P2's
      `findByServer()` is a free `list()` + filter, no storage change.
- [ ] **Parse `beamd.yaml` into a struct that tolerates unknown keys.** P2 just
      adds a `Server` field; a P1 client meeting a future `server:` file
      ignores it and falls back (forward-compatible) instead of erroring.
- [ ] **`~/.beamd/config` is an extensible struct** (`current` + room for
      `defaults`, and later `trusted_servers`), not a bare value.
- [ ] **One "identity missing" hook.** P1 says "run `beamd login --profile
      X`." P2 enriches the *same* hook with server-matching + device-code +
      the trust prompt — one place to grow, not N call sites.

**File story (no format change, no new filenames):** the project file is
**always `beamd.yaml`**, distinguished by *content*, not name:
- `beamd.yaml` with `profile:` (a personal alias) → personal; gitignore it.
- `beamd.yaml` with `server:` + naming → shareable; **commit it** — this *is*
  the "team" file, it just keeps the plain name.
- `beamd.local.yaml` → personal override, always gitignored, overrides `beamd.yaml`.

We suffix the **personal** file (`beamd.local.yaml`), following `.env` /
`.env.local` — **not** a `beamd.team.yaml` / `beamd.shared.yaml`. Rationale: zero new
convention to learn; the *override* is the special/local one, so it gets the
suffix; and "personal overrides shared" falls out for free (the precedence
ladder already lets `--profile` / `BEAMD_PROFILE` / `current` beat the
committed file). `beamd.local.yaml` is just the persistent, project-scoped form
of that override (a small P2 addition to discovery: load `beamd.yaml`, then merge
`beamd.local.yaml`). Distinct from `~/.beamd/` — your *global* identity/secrets,
never in a repo.

## Open questions (only the genuinely unresolved)

1. **`branch`-named *detached/always-on* tunnels.** A branch switch changes
   the label, potentially orphaning the old tunnel. Fine for ephemeral
   foreground previews; flag it in docs. Worth a guard (warn/auto-close on
   branch change) or just documentation? (Not a blocker.)
   - **Leaning: reclaim, don't guard.** Portless's answer to the same problem
     is PID-liveness + a `prune` command (see deep-dive §8): each detached
     tunnel records the owning PID; `list`/`status` mark entries whose process
     is gone, and `beamd prune` reaps them (and closes their tunnels). That's
     simpler and more general than watching for branch changes — it handles
     crashes and reboots too. Treat "branch switch orphans a tunnel" as one
     case of the broader "owner is gone" problem and solve it once with
     `prune` (a small P2 add to §5).

*(Resolved: `beamd.yaml` supports **both** `profile:` (personal/gitignored, P1)
and `server:` (committed/team, P2) — see §3.)*

---

## Why this stays simple (the anti-tunnel-vision checks)

- **Pain-first:** §1 alone removes the login churn; §2–§5 are additive.
- **No DSL:** naming is two crisp flags — `--as` (literal) or `--from` (a
  4-source menu). `beamd init` teaches it; nothing to memorize.
- **Secrets never leave the global store**, so project files are shareable.
- **Automation is untouched** — Flow keeps using `--config` + `--as`.

---

## Appendix — edge hardening  `[separate track, not profiles/naming]`

From the same portless read (deep-dive §4, §10). These touch `beamd serve`
(the edge), not the client, so they're independent of §1–§6 and can land
whenever. Captured here so they aren't lost.

- [ ] **HTTP/2 stream-reset tolerance.** If the edge terminates H2, a tunneled
      HMR/dev server triggers a flood of `RST_STREAM`s; Node-style servers send
      `GOAWAY INTERNAL_ERROR` after ~1000 and the session dies, surfacing as
      `ERR_HTTP2_PROTOCOL_ERROR` in Chrome. Confirm our edge's H2 settings
      tolerate high reset rates (portless sets `streamResetBurst:10000,
      streamResetRate:100`). Likely the first bug a real Vite user hits.
- [ ] **Loop guard.** Increment a hop header (e.g. `beam-hops`) on each pass;
      reject at a small ceiling (portless uses 5) with a page explaining the
      `changeOrigin: true` fix. Stops an app that fetches its own public URL
      from looping the tunnel to death.
- [ ] **Socket-error absorption.** Swallow ECONNRESET/EPIPE from abrupt client
      disconnects (tab close, reload, HMR) so they don't crash the edge; on a
      mid-stream upstream failure, `RST` the stream rather than half-closing
      (avoids a content-length mismatch Chrome treats as a session error).
- [ ] **Branded edge error pages.** Replace bare gateway errors with: a **404**
      that lists the caller's active tunnels + the `beamd open …` to start the
      missing one; a **502** that distinguishes "backend crashed" (connection
      refused) from "no backend yet." This is the highest-visibility UX polish —
      it's what a developer sees when something's wrong. (portless `pages.ts`.)
