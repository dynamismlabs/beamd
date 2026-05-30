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
- **No naming DSL.** Strategies are a small fixed menu chosen via a
  documented flag / config keyword (`--name-from dir`), not a `@token`
  template. Composition (e.g. `myapp-api`) is a literal name, not syntax.
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

## 2. Tunnel naming — flags + config, no DSL  `[P1]`

The label is set by, in precedence order: `--as` → `--name-from` → project
`.beamd` → global default → built-in `port`.

- **`--as <label>`** — an explicit literal label (always literal; covers
  composed names like `myapp-api`).
- **`--name-from <source>`** — derive the label. Sources (fixed menu):
  - `port` — the port number (the built-in default).
  - `dir` — basename of the current directory (covers pwd *and* worktree dirs).
  - `repo` — git repo name (`basename` of `git rev-parse --show-toplevel`).
  - `branch` — current git branch, sanitized (`feat/x` → `feat-x`).
- Config keys (global `~/.beamd/config` and project `.beamd`): `name`
  (literal) **or** `name_from` (one of the sources above).

**Tasks**
- [ ] `--as` and `--name-from` flags on `open` and `run`.
- [ ] Derivation functions for `dir` / `repo` / `branch` (+ `port`), each
      run from the invocation's cwd, then **sanitized to a single valid
      RFC 1123 label** (lowercase, alnum+hyphen, ≤63, collapse/strip
      invalid). Reuse `naming.ValidateLabel`; if a source can't be made
      valid (e.g. detached HEAD for `branch`, not in a git repo for
      `repo`), fail with a clear, actionable error.
- [ ] Name resolution helper wired into `open`/`run` following the ladder.
- **Acceptance:** in `~/work/myapp`, `beamd open 3000 --name-from dir` →
  `myapp.<slug>.<base>`; `--name-from branch` on `feat/x` → `feat-x.…`;
  `--name-from repo` outside a git repo errors clearly; with nothing set the
  label is still the port; `--as web-api` yields exactly `web-api`.

---

## 3. Project context `.beamd`  `[P1]`

A per-project file that supplies the profile and/or naming default, found by
**walking up from cwd** to the first `.beamd` (stop at `$HOME` or the
filesystem root). YAML, **no secrets**:

```yaml
# .beamd
profile: acme              # references a global profile by name (no token)
name_from: repo            # or:  name: myapp-api
```

- [ ] Discover `.beamd` by walking up from cwd; parse `profile`, `name`,
      `name_from`. Feed `profile` into §1's resolver and `name`/`name_from`
      into §2's.
- [ ] If `.beamd` names a profile that doesn't exist locally, error with
      "this project uses profile `acme` — run `beamd login --profile acme`".
- [ ] Document the sharing convention: a `.beamd` is **personal by default**
      (treat like `.env` — gitignore it), since `profile:` is a local alias.
      Teams that want to commit a shared one should align profile names.
- **Acceptance:** with a `.beamd` of `{profile: acme, name_from: repo}` in
  the repo, a bare `beamd open 3000` run anywhere in that tree uses the
  `acme` edge and names the tunnel after the repo — no flags. Flags still
  override it; running outside the tree falls back to `current` + `port`.

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

1. **`.beamd` sharing model.** Default is **personal + gitignored**, with
   `profile:` as a local alias (simplest, like `.env`). The alternative for
   *committed/team* configs is to reference the **server** (`server:
   tunnel.acme.com`, globally unique) and match the local profile by server,
   sidestepping the "your `acme` ≠ my `acme`" alias trap. Server-matching is
   a clean later add; **confirm whether the blessed path is personal-alias
   or committed-server.**
2. **`branch`-named *detached/always-on* tunnels.** A branch switch changes
   the label, potentially orphaning the old tunnel. Fine for ephemeral
   foreground previews; flag it in docs. Worth a guard (warn/auto-close on
   branch change) or just documentation? (Not a blocker.)

---

## Why this stays simple (the anti-tunnel-vision checks)

- **Pain-first:** §1 alone removes the login churn; §2–§5 are additive.
- **No DSL:** naming is a 4-item menu behind a documented flag, plus literal
  `--as`. `beamd init` teaches it; nothing to memorize.
- **Secrets never leave the global store**, so project files are shareable.
- **Automation is untouched** — Flow keeps using `--config` + `--as`.
