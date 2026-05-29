# After manual smoke testing — what's next

You've done the [Tier 1 setup](setup.md) and confirmed it works end-to-end
in your browser. This doc is the playbook for everything after that:
catch bugs, tag a release, plug remaining test gaps, decide what's next.

Pick steps in order. Stop wherever feels right; nothing past Step 2 is
required to use Beamd yourself.

---

## 0. Did the manual test surface bugs?

If yes — for each one, **write a failing test before you touch the fix.**
That's how the suite was built: every test in `test/e2e/` exists because
something either broke or *could* have broken in that specific way.

```
1.  Reproduce the bug in a minimal test (RED).
2.  Confirm the test fails the way you expect.
3.  Fix the bug.
4.  Confirm the test now passes (GREEN).
5.  Run `go test ./...` to make sure nothing else regressed.
```

This is the single highest-value habit you can build. It keeps the suite
honest: every test corresponds to a real concern, not "what would feel
thorough."

---

## 1. Run the bundled smoke test

`scripts/smoke-test.sh` exercises the proxy hop with the test backend
this repo ships (`cmd/beam-testapp`). It hits routes that cover the
proxy's interesting paths:

| Check | What it proves |
|---|---|
| `GET /` returns the testapp banner | TLS works, routing works, backend reachable |
| `X-Forwarded-For` reaches backend | Header forwarding is set up |
| `X-Forwarded-Proto = https` | Proxy advertises HTTPS to the backend |
| `X-Forwarded-Host` matches the tunnel host | Host preserved |
| `POST /echo` returns body verbatim | Request body forwarded byte-for-byte |
| `GET /size?bytes=8192` returns 8192 bytes | Larger responses don't truncate |
| `GET /sleep?ms=1000` succeeds | Latency-tolerant; proxy doesn't time out short backends |

Run it from the repo root after every beamd change:

```
make smoke-test
```

Or, with binaries already on PATH:

```
./scripts/smoke-test.sh
```

It cleans up after itself (kills the test app, unexposes the tunnel).

---

## 2. Tag v0.1.0

Once the smoke test passes against your real deployment:

```bash
# Update / create CHANGELOG.md — see template below.
$EDITOR CHANGELOG.md

# Tag and push.
git tag -a v0.1.0 -m "first usable release"
git push origin v0.1.0
```

GoReleaser (`.goreleaser.yaml`) will then:

- Build cross-platform binaries (linux/darwin × amd64/arm64) for
  `beamd` and `beam-testapp`.
- Bundle them with `README.md`, `LICENSE`, `example/beamd.yaml`,
  `scripts/smoke-test.sh`, and the two docs.
- Push `ghcr.io/dynamismlabs/beamd:{v0.1.0,latest}` to GHCR.
- Create a draft GitHub release.

After the release lands:

- Edit `README.md` and `docs/setup.md` to remove the "until v0.1.0 is
  tagged" caveats.
- Test the published image: `docker pull ghcr.io/dynamismlabs/beamd:v0.1.0`.

### CHANGELOG.md template

Standard "Keep a Changelog" format. Start with:

```markdown
# Changelog

## [0.1.0] — YYYY-MM-DD

First usable release.

### Added
- TLS+yamux multiplexed tunnel; one client connection carries many tunnels.
- NDJSON control protocol on a dedicated stream (hello, register,
  unregister, heartbeat, error).
- Per-developer wildcard certs via Let's Encrypt (DNS-01 over the libdns
  Cloudflare provider).
- `beamd` CLI: login (copy-paste), up, list, down.
- Local agent over unix socket with auto-start.
- MCP stdio server exposing `expose_port`, `unexpose`, `list_tunnels`
  to AI agents.
- Reconnect with exponential backoff + replay of every active
  registration; URLs stay stable across blips.
- `/metrics` Prometheus endpoint and structured per-request slog logs.
- Graceful shutdown that notifies clients (`error{code:"shutdown"}`)
  so they reconnect immediately.

### Known limitations
- Operator must onboard each developer manually (copy-paste token).
- Only Cloudflare and a test stub are wired DNS providers.
- Unix-only agent (no Windows named pipe yet).
- Real ACME path is exercised manually; no automated CI test against
  Pebble or LE staging.
```

---

## 3. Plug the highest-value test gaps

Listed in priority order. None of these block v0.1.0 — they're the
things to do *after* the first release lands.

### 3a. Real ACME via Pebble

**Why it matters most:** the cert path is the one component we ship
that has zero automated end-to-end coverage. Every other layer is
exercised by `test/e2e/`; the ACME dance (challenge → propagation →
issuance → chain validation) has never been run in CI.

**Plan:**

1. Add a build-tagged integration test file:
   `test/integration/acme_pebble_test.go` with `//go:build integration`.
2. Make CI start [Pebble] in Docker for that build tag:

   ```
   docker run -d --name pebble \
     -e PEBBLE_VA_ALWAYS_VALID=1 \
     -p 14000:14000 -p 15000:15000 \
     letsencrypt/pebble \
     pebble -dnsserver localhost:53 -strict
   ```

3. Test plan: configure `MagicManager` with `acme_ca:
   https://localhost:14000/dir`, use `dns.StubProvider` so the
   DNS-01 challenge writes succeed in-memory, trigger
   `mgr.PreWarm("turing")`, assert no error + cert chain validates
   against Pebble's root (fetched from `:15000/roots/0`).

This is the single highest-confidence test you can add — it proves
the whole MagicManager path works without touching real LE.

[Pebble]: https://github.com/letsencrypt/pebble

### 3b. Streaming responses

**Why:** SSE, long-poll, large file downloads. All of these silently
break if the proxy buffers the whole response before forwarding (which
our test suite would NOT catch — current tests only check small bodies).

**Plan:** new test in `test/e2e/` that opens an HTTP response to
`beam-testapp`'s `/sse` route through the tunnel and asserts:

- First event arrives in &lt;1s.
- All 5 events arrive within ~3s with sub-second gaps.
- Each tick's body matches `data: tick N ...`.

### 3c. POST body byte-correctness

**Why:** the smoke-test covers small bodies. We don't test a non-trivial
payload, and "the proxy strips the last 4 bytes" or "rewrites a header
that corrupts content" would slip through.

**Plan:** generate 64 KiB of random bytes, POST to the tunnel,
backend records the body, assert SHA-256 matches.

### 3d. In-flight request during reconnect

**Why:** today's reconnect test confirms a *new* request after
reconnect works. It doesn't confirm a request *in progress* at the
moment the edge drops behaves well — could hang forever, could return
truncated body.

**Plan:** start a `GET /sleep?ms=2000` request, after 500ms call
`e.CloseAllSessions()`, assert the request either completes (200
with `slept 2000 ms`) or fails cleanly with a 5xx — never a hang.

---

## 4. Add CONTRIBUTING.md

Short doc covering:

- How to build (`make build`) and test (`make test`).
- How to add a libdns provider (one import + one switch case in
  `internal/dns/dns.go`).
- Style: `gofmt`, no other linters.
- How `TASKS.md` works (it's the working checklist mapped to PRD
  milestones).
- The "write a regression test for every bug" rule from §0 above.

---

## 5. Decide whether to pursue Tier 2

Tier 2 = sell to 5–30 paying customers, all onboarded manually by you.
Real validation that someone will pay before you build the hosted
product (Tier 3).

Lowest-effort path (cumulative, ~1 weekend):

1. Pick a payment processor — **Stripe Payment Links** is by far the
   simplest. Create one link, pin a price.
2. Build a one-page signup form. Options from least to most work:
   - A Tally / Google Form.
   - A Formspree-backed static HTML page on Cloudflare Pages.
3. Per signup, run the four commands from
   [`docs/setup.md`'s Step 14](setup.md#step-14--onboard-a-teammate)
   and email the user their token + link to their tunnel.
4. Track usage via the edge's `/metrics`. If a single slug's
   `beam_bytes_proxied_total` spikes, investigate.

**Ceiling:** ~30 customers before you're spending a noticeable chunk
of every day onboarding. That's actually a great milestone — you've
validated demand and now have a real reason to build Tier 3.

---

## 6. When (and only when) Tier 2 says "yes" — start Tier 3

Tier 3 is the hosted product. The work bucket:

- **Device-code login** (PRD §17, TASKS.md deferred section) — server
  endpoints + client polling + admin confirmer. Estimated 1-2 focused
  hours of code.
- **A signup site** — separate from this repo. Could be Next.js on
  Vercel + Stripe Checkout. A few days of work.
- **Multi-tenant token store** — replace `tokens.json` with a
  Postgres-backed `auth.Store`. Slot it in behind the existing
  interface; no other code changes.
- **Customer dashboard** — see your slug, regenerate tokens, view
  active tunnels.
- **Operational tooling** — Prometheus alerts on cert renewals,
  Sentry/Honeycomb for errors.

The estimated total is genuinely "a few weeks" if you're focused. The
gap from "I sell manually to 20 customers" to "anyone signs themselves
up" is well-understood, no architectural unknowns left.
