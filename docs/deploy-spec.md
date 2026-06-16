# Spec — Deploying beamd (zero → working, by effort level)

> **Status:** design + build plan. Captures the goal, what's automatable, the
> hard limits, and the steps a user follows from nothing to a working edge.
> Pairs with [`setup.md`](setup.md) (the detailed manual path) and
> [`hosted-mode.md`](hosted-mode.md) (the *we*-run-it path).

## Goal

Make standing up a self-hosted beamd edge as low-effort as the user wants it to
be — and **signal that clearly**. Offer a spectrum, not one path: hand-holding
docs, one-command automation, or hand the whole thing to an agent. The same
underlying steps; different amounts of "you" vs "us."

## What "working" means (definition of done)

A deploy is done when, from a laptop:

```
beamd login --server <base_domain> --token <token>
beamd open 3000            # → https://3000.<base_domain> resolves over HTTPS
```

That requires, on the server: the edge running on `:443`, DNS `A`/`AAAA` for
`<base_domain>` (+ wildcard) pointing at it, a Let's Encrypt cert issued via
DNS-01, and one developer token minted.

## The two things only *you* can do (irreducible)

No platform, script, or agent removes these:

1. **Own a domain** (the `base_domain`, e.g. `tun.acme.com`).
2. **Create a DNS provider API token** with write access (Cloudflare
   `Zone.DNS:Write` today). Used for *both* writing records *and* DNS-01 cert
   issuance.

Everything after these two inputs is automatable. (The only way to remove even
these is **hosted mode** — we own the domain + edge, you just `beamd login`.)

## The networking constraint (where beamd can run)

beamd terminates its **own** TLS and ALPN-demuxes the control plane vs public
HTTPS on a single `:443`. So it needs a **raw TCP `:443` on a public IP** — it
cannot sit behind a platform that terminates TLS / routes L7 HTTP. This is the
whole story of platform fit (and is a *hosting* property, not a Docker one):

| Target | Fits? | Why |
|---|:---:|---|
| Any VM — DO droplet, Hetzner, Linode, Vultr, EC2, GCE | ✅ | you are the host; raw `:443` + public IP |
| Fly.io | ✅ | raw-TCP passthrough (`handlers = []`) + a dedicated IP |
| Render, Railway, DO App Platform, Heroku, Cloud Run | ❌ | they terminate TLS in front of your container |
| Cloudflare Workers / Pages / Containers | ❌ | request-scoped, no raw `:443` + custom TLS/ALPN |

**Blueprint consequence:** only Fly (a managed *app* platform) needs its own
file (`fly.toml`). Every VM uses **`cloud-init`**, and *one* cloud-init file
works across every VM cloud. So there are really **two** blueprint artifacts to
maintain, not a dozen. Docker is fine on either — the container is portable; the
raw-`:443`-and-IP requirement is what gates portability.

> **Cloudflare clarified:** it's a *DNS provider* (cert issuance + records), not
> a host. You can't run beamd on Cloudflare compute. And DNS is pluggable
> (libdns) — Cloudflare is just the only provider compiled in today; Route53 /
> DigitalOcean DNS / etc. are ~20 lines each to add.

## Three paths, by effort

Pick by how much the user wants to do themselves. All three reach the same
"working" state and share the same underlying steps.

### Path A — Hand it to an agent  *(lowest effort)*
The **`deploy-beamd` skill** (for Claude Code / any agent). The user pastes a
prompt; the agent asks the three real questions (domain, DNS token location,
where to host + region), then provisions, writes DNS, runs the bootstrap, and
verifies. **Secrets never enter the chat** (see below). Best for "just do it for
me." Degrades gracefully — works against today's `init`/`add-developer` even
before `bootstrap` ships.

### Path B — Blueprint / one command  *(low effort)*
For "I'll run a couple commands myself."
- **Fly:** copy `fly.toml` → `fly launch` → `fly ips allocate-v4` → `fly secrets
  set BEAMD_DNS_PROVIDER_CREDS=…`.
- **Any VM:** paste one `cloud-init.yaml` at droplet-create (or a DO Marketplace
  1-click image / pre-filled deep link), **or** `curl -fsSL https://get.beamd.run
  | bash` on a box you already have.

### Path C — Guided manual  *(most control)*
[`setup.md`](setup.md), step by step. Full visibility, nothing hidden. Best for
the security-conscious or for understanding the moving parts.

## The canonical zero→working flow (annotated)

Every path runs these steps; they differ only in who/what executes them.

| # | Step | Who | Automatable? |
|---|---|---|---|
| 1 | Get a server with a public IP | user picks; script/agent can provision | ✅ (or bring your own) |
| 2 | Install beamd (binary or image) | install route below | ✅ |
| 3 | Provide `base_domain` | **user** | ❌ (must own it) |
| 4 | Provide DNS API token | **user** (by reference — never chat) | ❌ (your secret) |
| 5 | Detect the server's public IP | metadata / `ifconfig.me` | ✅ |
| 6 | Write `A`/`AAAA` for base + wildcard | via the DNS token | ✅ |
| 7 | Write `beamd.yaml` config | `beamd init --non-interactive` | ✅ |
| 8 | Start the edge on `:443` | systemd / `docker compose up -d` | ✅ |
| 9 | Issue + pre-warm the cert (DNS-01) | `beamd add-developer` / pre-warm | ✅ |
| 10 | Mint the first developer token | `beamd add-developer` | ✅ |
| 11 | `beamd login` from a laptop | **user** (one paste) | ❌ (trivial) |

So the user's actual surface is **steps 3, 4, and 11** — a domain, a token, and
one `login`. Steps 5–10 are the `beamd bootstrap` one-shot (planned).

## Getting the binary

`beamd` is one binary (edge *and* client). Install routes, by context:

| Route | Command | Needs | Best for |
|---|---|---|---|
| **npm** | `npm i -g @beamd/cli` | Node on the box | dev laptops (the client); quick edge if Node's already there |
| **Go** | `go install github.com/dynamismlabs/beamd/cmd/beamd@vX.Y.Z` | Go toolchain | Go users |
| **Docker** | `ghcr.io/dynamismlabs/beamd:latest` | Docker | the **edge** — self-contained, no runtime deps |
| **Installer** | `curl -fsSL https://get.beamd.run \| bash` | nothing | one-shot VM setup |

> So yes — **`npm i -g @beamd/cli`** is the canonical *client* install, and works
> for the edge too if the box has Node. For a server we'd steer toward the
> **Docker image** or the static-binary installer, so running a Go binary
> doesn't drag in a Node runtime.

## Secrets handling (no tokens in chat)

beamd reads the DNS token from an **env var** (`BEAMD_DNS_PROVIDER_CREDS`), never
from chat or the committed config. The rule for the skill and the docs:
**reference secrets by name, never by value.**
- Fly: the *user* runs `fly secrets set BEAMD_DNS_PROVIDER_CREDS=…`.
- VM: the user pastes the token into `/etc/beamd/.env` over their own SSH (or
  the cloud's secret/metadata injection). In Claude Code, the `!` prefix runs
  the set-command in the user's session without the value entering the model.
- The agent collects everything *except* the token in conversation; for the
  token it references the env var / secret store and never echoes it.

## Limitations (state them plainly)

- **HTTP-terminating PaaS won't work** (Render/Heroku/App Platform/Cloud Run) —
  beamd needs raw `:443`. Don't ship buttons there.
- **No Cloudflare-compute hosting** — CF is DNS only.
- **DNS = Cloudflare only today** (pluggable; more providers are small adds).
- **You must own a domain.**
- **A single edge is your uptime** — no built-in HA/anycast/DDoS; one box, one
  region until you run more. (Hosted mode is where that gets solved.)
- **You operate it** — cert renewal is automatic, but the box, upgrades, and
  monitoring are yours.

## What we build to make this real (deliverables + status)

Ordered so each unblocks the next; the first three already give a strong "easy"
signal.

- [ ] **Publish the image** to GHCR on tag — the unblocker for every pull-based
      path (today it's built locally only).
- [ ] **`deploy-beamd` agent skill** — input-gathering + secrets-by-reference +
      run + verify. Highest signal, cheapest; works against today's commands.
- [ ] **README "Deploy" section** — the three paths + the honest fit matrix.
- [ ] **`beamd bootstrap` subcommand** — steps 5–10 as one flag-driven command
      (auto-detect IP, write A records, `init`, mint token, pre-warm). Makes
      every blueprint ~10 lines. (Reuses existing `init` + `add-developer`.)
- [ ] **`fly.toml` + `cloud-init.yaml`** — the two blueprint artifacts (PaaS +
      all VMs); optionally a DO Marketplace image / deep-link wrapping the
      cloud-init.
- [ ] **`get.beamd.run` installer** — for "I already have a box."
- [ ] (later) **A second DNS provider** (Route53 or DigitalOcean DNS) to prove
      pluggability and widen reach.

**Today:** the manual path ([`setup.md`](setup.md)) + the Docker image work end
to end (the live demo runs this). Everything above is polish that moves users
down the effort curve — and even shipping just the **skill + published image +
this doc** signals "we make it easy" without building all of it.
