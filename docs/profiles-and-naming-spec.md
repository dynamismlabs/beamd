# Spec — Profiles, project context, and tunnel naming

> Self-contained build plan. Each item is a checkbox with concrete
> acceptance. You shouldn't need anything beyond this file + the codebase.

## 0. Background & goal

beamd's client today logs into **one** edge (`~/.beamd/config` = a single
`{server, token}`), and tunnel names are the port number unless you pass
`--as`. Two problems:

1. **Login churn.** Working across companies/projects means logging out and
   back in to switch edges. We want to be logged into **many at once** and
   select seamlessly — the kubectl/`gh auth`/AWS-profile model.
2. **Naming is rigid.** People want the label to come from the port, the
   folder, the git repo, or the branch — and have project-level defaults.

**Unifying idea.** Every `beamd open` resolves two contexts — *which edge*
(→ `<slug>.<base>`) and *what name* (→ `<label>`). They're the same shape of
problem, so they share **one precedence ladder** and one project file.

**Decisions locked (apply throughout):**
- **One ladder** (highest wins): `CLI flag` → `env` → project `.beamd`
  (nearest, walking up from cwd) → global config → built-in default.
- **Secrets are global-only.** A `.beamd` file references a profile/server;
  it never contains a token. That's what makes it safe to share.
- **No naming DSL.** Strategies are a small fixed menu of keywords on the
  one `--as` flag (`--as dir`), not a `@token` template. Composition (e.g.
  `myapp-api`) is just a literal `--as` value, not syntax.
- **Default name stays `port`.** Nicer naming is opt-in, so no URL changes
  out from under anyone.
- **The programmatic path is unaffected.** Automation (Flow) passes
  `--config <path>` + `--as <name>` explicitly and bypasses all of this;
  profiles / `.beamd` / strategies are human-CLI ergonomics. (`--config`
  takes precedence over `--profile`.)

**Suggested order:** §1 → §2 → §3 → §4 → §5.

---

## 1. Profiles — many identities, zero logout churn  `[P0 — the pain-killer]`

Be logged into every edge at once; switch with a flag, env, or a default.

**Storage** (reuses the existing client-config loader):
- `~/.beamd/profiles/<name>` — one file per profile, the existing client
  config YAML (`server`, `token`, …).
- `~/.beamd/config` — top-level: `current: <name>` + global defaults
  (e.g. `name_from`).
- Per-profile agent socket lives at `~/.beamd/agents/<name>.sock` (used by §5).
- **Backward compat:** a legacy `~/.beamd/config` containing `server`/`token`
  is migrated to profile `default` (and `current: default`) on first run —
  existing users keep working unchanged.

**Tasks**
- [ ] Profile store: load/save `profiles/<name>`, read/write the `current`
      pointer, and the one-time legacy-config → `default` migration.
- [ ] Profile resolution helper: `--profile/-p <name>` → `BEAMD_PROFILE`
      env → `.beamd` `profile:` (§3) → `current`. An explicit `--config
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

## 2. Tunnel naming — one `--as` flag, no DSL  `[P1]`

The label is set by, in precedence order: `--as` → project `.beamd` →
global default → built-in `port`.

**`--as <value>`** accepts either a **reserved keyword** (derives the
label) or anything else (used **literally**):

| `--as` value | result |
|---|---|
| *(omitted)* | `port` — the port number (default) |
| `port` | the port number |
| `dir` | basename of the current directory (covers pwd *and* worktree dirs) |
| `repo` | git repo name (`basename` of `git rev-parse --show-toplevel`) |
| `branch` | current git branch, sanitized (`feat/x` → `feat-x`) |
| `web-api` (anything else) | used **literally** |

One flag, short and idiomatic (cf. `docker --restart always|no`). The
config key is a single `name:` (same keyword-or-literal rule) in both the
global config and `.beamd`. The 4 keywords are words no one uses as an app
name, so the literal-vs-keyword ambiguity is theoretical; `--help` and
`beamd init` make it explicit.

**Tasks**
- [ ] `--as` on `open` and `run`, interpreting the reserved keywords vs a
      literal.
- [ ] Derivation functions for `dir` / `repo` / `branch` (+ `port`), each
      run from the invocation's cwd, then **sanitized to a single valid
      RFC 1123 label** (lowercase, alnum+hyphen, ≤63, collapse/strip
      invalid). Reuse `naming.ValidateLabel`; if a source can't be made
      valid (e.g. detached HEAD for `branch`, not in a git repo for
      `repo`), fail with a clear, actionable error.
- [ ] Name resolution helper wired into `open`/`run` following the ladder
      (flag → `.beamd` `name:` → global `name:` → `port`).
- **Acceptance:** in `~/work/myapp`, `beamd open 3000 --as dir` →
  `myapp.<slug>.<base>`; `--as branch` on `feat/x` → `feat-x.…`; `--as
  repo` outside a git repo errors clearly; with nothing set the label is
  the port; `--as web-api` yields exactly `web-api`.

---

## 3. Project context `.beamd`  `[P1 personal · P2 team]`

A per-project file (YAML, **never a secret**) supplying the identity and/or
naming default, found by **walking up from cwd** to the first `.beamd`
(stop at `$HOME` or the filesystem root). Two ways to point at an edge:

```yaml
# personal .beamd — gitignore it, like .env
profile: acme             # references a global profile by name
name: repo                # keyword or literal (see §2)
```
```yaml
# shared/committed .beamd — references the edge canonically
server: tunnel.acme.com   # globally unique; matched against your profiles
name: repo
```

- [ ] **(P1)** Discover `.beamd` by walking up from cwd; parse
      `profile`/`server` and `name`. `profile:` → that named profile; if it
      doesn't exist locally, error "this project uses profile `acme` — run
      `beamd login --profile acme`". Personal/gitignored convention.
- [ ] **(P2)** `server:` matching — resolve to the local profile whose
      server matches, so a *committed* file works for any teammate
      regardless of what they named their profile. If none matches, guide
      login: "this project tunnels through `tunnel.acme.com` — run `beamd
      login`" (offer the browser/device-code flow when the edge advertises
      it).
- [ ] **(P2) First-use trust.** The first time a `.beamd` points your client
      at a **new** server, confirm before connecting ("This project wants to
      tunnel through `tunnel.acme.com` — allow? [y/N]") and remember the
      answer. A committed file silently redirecting your local ports to an
      arbitrary edge is a real risk; one y/n closes it without nagging.
- **Acceptance (P1):** a personal `.beamd` `{profile: acme, name: repo}` →
  bare `beamd open 3000` anywhere in the tree uses `acme` + the repo name;
  flags override; outside the tree it's `current` + `port`.
- **Acceptance (P2):** a committed `.beamd` `{server: tunnel.acme.com, name:
  repo}` → a teammate already logged into that edge (under any profile name)
  gets the right edge + repo-named tunnel after one trust prompt; someone
  not logged in is guided to log in.

### Why a shared `.beamd` is a growth lever (and how to earn it)

A committed `.beamd` carries the team's institutional knowledge — *which
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
  `.beamd`'s `server:` form now (above) to keep that door open.
- **Trust has to be explicit** (the first-use confirmation above) — otherwise
  a shared config is a vector for redirecting someone's traffic.

For the hosted product this doubles as the funnel: a committed `.beamd` +
device-code login means each teammate who clones becomes a signed-in,
attributable user on the org's account.

---

## 4. Guided setup `beamd init`  `[P2]`

So the first run is taught, not looked up.

- [ ] `beamd init` (interactive, in a project dir): pick a profile (from
      existing, or "log in to a new one" → runs §1 login), pick a naming
      source (`port`/`dir`/`repo`/`branch`/custom literal), then write
      `.beamd`. Reuse the prompt UX.
- [ ] Non-interactive: `beamd init --profile <name> [--name-from <src> |
      --as <literal>]`.
- **Acceptance:** `beamd init` in a fresh project writes a valid `.beamd`;
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

## Open questions (only the genuinely unresolved)

1. **`branch`-named *detached/always-on* tunnels.** A branch switch changes
   the label, potentially orphaning the old tunnel. Fine for ephemeral
   foreground previews; flag it in docs. Worth a guard (warn/auto-close on
   branch change) or just documentation? (Not a blocker.)

*(Resolved: `.beamd` supports **both** `profile:` (personal/gitignored, P1)
and `server:` (committed/team, P2) — see §3.)*

---

## Why this stays simple (the anti-tunnel-vision checks)

- **Pain-first:** §1 alone removes the login churn; §2–§5 are additive.
- **No DSL:** naming is the one `--as` flag — 4 reserved keywords or a
  literal. `beamd init` teaches it; nothing to memorize.
- **Secrets never leave the global store**, so project files are shareable.
- **Automation is untouched** — Flow keeps using `--config` + `--as`.
