# Identity, accounts & scope

> **Status:** canonical decision. This is the single source of truth for how a
> beamd client authenticates, stores credentials, and decides *where a tunnel
> lands* (which server, which org). `hosted-mode.md` defines the server-side
> contract that backs it; `profiles-and-naming-spec.md`'s identity half is
> **superseded** by this doc (its naming / `.beamd` / `run` half still stands).
>
> The CLI as shipped today still implements the older *profiles + `beamd use`*
> model. The migration to what's below is tracked in
> [`cli-identity-plan.md`](cli-identity-plan.md).

## The model in one sentence

**Your machine is logged into beamd as you (per server); you can act as any of
your orgs; a project pins which org/edge it uses; automation uses a
workspace-scoped API key instead of your login.**

Everything below follows from separating three layers that the old "profile"
concept conflated:

| Layer | What it is | How many | Set by |
|---|---|---|---|
| **Account** | a credential bound to **a beamd server** | usually **1**; more only with multiple beamd instances | `beamd login` |
| **Scope** (org) | which org you're **acting as**, *within* one login | all the orgs you belong to | a selector, not a credential |
| **Project** | a `.beamd` file pinning `{server, scope, name}` for a repo | per project | committed config |

beamd is `gh` × Vercel: **per-server login** (like `gh`'s multi-host) **plus
per-org scope within a login** (like `vercel --scope`), degrading to a plain
`{server, token}` for OSS self-host.

## Two credentials, split by purpose

There is exactly **one** thing the edge validates — a bearer token. It is
*minted* two ways, for two different principals. This is the Vercel / GitHub /
Stripe split, and keeping the two jobs separate is what makes it both simple
and secure.

| | **Interactive login** (`beamd login`) | **API key** (dashboard-issued) |
|---|---|---|
| Principal | **you** (the user) | **a workspace** (the org) |
| Reaches | **all orgs you belong to** | exactly one workspace |
| Pick scope | per-command / `.beamd` / your default — **no re-login** | fixed; the key *is* the scope |
| Lifetime | session-grade: refreshable, revoked on logout, MFA/SSO-gateable | long-lived, named, independently revocable |
| Acquired via | **device-code** (browser approval — inherits every dashboard auth method) | **dashboard "Create API key"**, shown once |
| Used by | humans at a terminal | CI, agents, `--config` files |
| If it leaks | broad, but ephemeral + revocable + MFA-gated | one workspace — that's the ceiling |

**Rule:** broad ⇒ ephemeral & human; durable ⇒ narrow & org. Never
broad-and-durable (the classic-PAT footgun), never narrow-and-interactive (the
per-org-login annoyance).

The CLI exposes exactly **one** interactive login flow — device-code (press
enter, approve in the browser). The *variety* of auth methods (Google, GitHub,
magic-link, SSO, MFA) lives in the browser, not the terminal — so the CLI never
touches a password. Headless contexts (no browser) use an **API key**:
`beamd login --token <key>` or straight into a `--config` file.

## Where a tunnel lands — resolution

Two things must resolve for every `open` / `run`: **which server** and **which
scope**. Both use the same ladder; the project file supplies both:

```
1. --server / --scope flag        # this command   (explicit one-off; beats everything)
2. .beamd  (server + scope + name) # this repo       (committed, shared — the ONLY persistence)
3. default                         # your only/primary account · scope you set with `beamd default` (personal unless set)
```

**There is no `beamd use`.** There is no sticky, machine-global "current scope"
that you toggle day-to-day — that hidden, easily-stale mode is the footgun we
deliberately removed. The only *persistent* routing lives in the repo's
`.beamd`, where it's visible, committed, and shared.

### The one standing default — `beamd default`

For the developer who lives in one org, a per-account **default scope** (a
*set-once preference*, not a toggled mode) avoids repeating `--scope`:

```
beamd default acme      # make acme my default scope on this account
beamd default           # show the current default
```

- Defaults to **personal**. Most people never run it — `beamd login --scope
  acme` (or a pick at login when you belong to several orgs) sets it once.
- It is a **preference** (set once, rarely changed), not a *mode* (toggled
  constantly). That distinction is the whole reason it isn't a footgun.
- Always overridden by `--scope` and by a project `.beamd`, so inside real work
  it's irrelevant; it only applies to bare, projectless commands.
- Visible everywhere (`whoami`, `status`, and the scope is in every URL), so a
  standing default can never silently misroute you.

## Storage — one file per server

```
~/.beamd/
  config                          # global: default account + naming defaults
  accounts/
    beamd.ai.yaml                 # hosted → session + cached orgs + default scope
    edge.mycompany.com.yaml       # OSS → static token, flat
    client-edge.example.com.yaml  # OSS → static token, slug
  agents/
    beamd.ai.sock                 # one detached agent per account
    edge.mycompany.com.sock
    client-edge.example.com.sock
```

Accounts are keyed by **server host** (the primary handle). An optional name
disambiguates the rare case of two credentials for the *same* server — it is a
tiebreaker, not the primary key, so the common case stays nameless.

**Hosted account** (`accounts/beamd.ai.yaml`) — the only "rich" shape:
```yaml
server: beamd.ai
kind: session
session_token: <user session>
user: trey@example.com
scopes:                  # cached at login for the picker; the edge is source of truth
  - { slug: trey, role: owner }     # personal
  - { slug: acme, role: member }
  - { slug: beta, role: admin }
default_scope: acme      # `beamd default` · personal unless set
```

**OSS account** (`accounts/edge.mycompany.com.yaml`) — identical to today:
```yaml
server: edge.mycompany.com
kind: token
token: <static>
# flat: no slug. (a namespaced edge adds `slug: client1`)
```

**Global** (`~/.beamd/config`):
```yaml
current: beamd.ai        # default account when no project/flag selects one
defaults: { from: branch }
```

## OSS vs hosted vs mixed — one primitive, varying richness

The edge resolves every credential to a **scope answer**. Three rows; the first
two are structurally identical (one fixed scope), so an OSS token *is* a hosted
API key from the edge's point of view:

| Credential | Where | Resolves to | Scope switching? |
|---|---|---|---|
| OSS static token | self-host (FileStore) | **one** slug (or flat `""`) | no — operator-assigned |
| Hosted API key | hosted (verify-token) | **one** workspace slug | no — the key *is* the scope |
| Hosted user session | hosted (verify-token) | **a set** of scopes | yes — `--scope` / default |

So **OSS auth doesn't change**: still `beamd login --server X --token Y`, no
device-code, no orgs, no scope set. Hosted simply *adds* the third row beside
it. A machine with hosted + two OSS edges is just an `accounts/` directory with
all three — selected by the same ladder, with the scope layer lighting up only
on accounts whose server supports it. On an OSS account, `beamd orgs` reports
"this server has no org concept (self-hosted)" and `--scope` is a no-op.

`beamd login` picks the flow by what the server advertises (the
`auth_discovery:` hook): a hosted server → device-code/browser → store a
session; a plain OSS edge → paste a token → store a static token. Same stored
shape either way.

## Command surface

| Command | Purpose |
|---|---|
| `beamd login [--server H] [--token K] [--scope S]` | hosted: device-code (browser) → session, sets default scope. OSS: store `{server, token}`. |
| `beamd logout [--server H]` | drop an account |
| `beamd default [scope]` | show / set this account's default scope (personal unless set) |
| `beamd whoami` | user + server + current scope |
| `beamd orgs` *(alias `scopes`)* | list orgs the current account can act in, mark default (hosted only) |
| `beamd accounts` | list servers you're logged into (advanced; hidden in the common single-account case) |
| `--server H` / `--scope S` | per-command override on `open` / `run` / etc. |

`beamd use` and `beamd profiles` are **removed** (replaced by `beamd default` +
`beamd orgs` / `beamd accounts`).

## Automation is unaffected

The programmatic path is unchanged: pass `--config <path>` with an
`{server, token}` (an **API key** — workspace-scoped) and it bypasses accounts,
scope selection, and the project ladder entirely. No `beamd default`, no
`--scope`, no device-code. This is exactly what Flow does today, and it keeps
working byte-for-byte. See [`consuming-beamd.md`](consuming-beamd.md).

## The verify-token contract (server side)

To support "one login, many orgs," `verify-token` distinguishes the credential
kinds and, for a user session, returns the **scope set** rather than a single
slug. Full wire contract in [`hosted-mode.md`](hosted-mode.md) §2.1:

- user session → `{ kind: "session", user, scopes: [{slug, role}, …] }`
- API key → `{ kind: "key", slug }`
- (OSS FileStore is unchanged — a single slug, like an API key.)

The edge caches the answer (~60s) and, on tunnel register, authorizes the
*requested* scope against the set. Picking a scope you've been removed from is
rejected at connect with an actionable message, not silently misrouted.
