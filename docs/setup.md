# Tier 1 setup — running Beamd for yourself or your team

End-to-end walkthrough. Follow it top-to-bottom; each step has a way to
verify it worked before moving on.

> **Cost:** ~$10–15/year (domain) + ~$5/month (small VM). Free Cloudflare
> + free Let's Encrypt for everything else.
>
> **Time:** ~30–60 minutes the first time, mostly waiting on DNS.

---

## Things to substitute

Throughout this doc I use placeholders. Pick your values now and keep
them in a scratch file:

| Placeholder | Meaning | Example |
|---|---|---|
| `YOUR_DOMAIN` | The hostname Beamd lives under (its `base_domain`). A dedicated apex domain (`mytunnel.dev`) **or** a subdomain of a domain you already run on Cloudflare (`beam.mydomain.com`). | `beam.mydomain.com` |
| `YOUR_ZONE` | The registered Cloudflare zone that owns `YOUR_DOMAIN`. Same as `YOUR_DOMAIN` for an apex; the parent for a subdomain. Beamd auto-detects this. | `mydomain.com` |
| `YOUR_SERVER_IP` | Public IPv4 of the VM you'll run beamd on | `203.0.113.42` |
| `YOUR_EMAIL` | Contact email for Let's Encrypt registration | `you@example.com` |
| `YOUR_SLUG` | *Optional* per-developer namespace (lowercase, alphanumeric, hyphens — RFC 1123 label). Omit for flat routing (`<name>.YOUR_DOMAIN`); set it only to namespace tunnels on a shared edge. | `turing` |
| `YOUR_TOKEN` | A long random secret you'll generate in step 7 | `4f2c…` (64 hex chars) |
| `YOUR_CF_TOKEN` | The Cloudflare API token you'll create in step 3 | `abc123…` (40 chars) |

---

## Step 1 — Buy a domain

> **Already have a domain on Cloudflare?** You can skip buying one and
> use a **subdomain** of it as `YOUR_DOMAIN` (e.g. `beam.mydomain.com`).
> Beamd auto-detects the parent zone (`mydomain.com`) and writes records
> relative to it. If you go this route, skip Steps 1–2, keep your CF
> token scoped to the **parent zone**, and in Step 5 add the A record at
> the subdomain label instead of `@`.

Get a fresh domain dedicated to Beamd. Cheapest options:

- [Porkbun] (~$10–12/year for `.dev`/`.com`)
- [Namecheap], [Cloudflare Registrar], whoever you already trust

[Porkbun]: https://porkbun.com
[Namecheap]: https://namecheap.com
[Cloudflare Registrar]: https://www.cloudflare.com/products/registrar/

**Why a fresh domain?** You'll point its *entire* DNS at the beamd
server. If you reuse a domain you already use for email/website, you'll
have to coexist with existing records — doable but more error-prone.

**Verify:** You can log into the registrar and see `YOUR_DOMAIN` in your
list.

---

## Step 2 — Point the domain at Cloudflare

We're using Cloudflare as the DNS host because Beamd's libdns/Cloudflare
adapter is the most mature.

1. Sign up at [cloudflare.com] (free).
2. Dashboard → **Add a Site** → enter `YOUR_DOMAIN` → pick the **Free**
   plan.
3. Cloudflare will scan for existing DNS records (none on a fresh domain
   — that's fine).
4. Cloudflare shows you **two nameservers** like
   `nina.ns.cloudflare.com` and `seth.ns.cloudflare.com`. Copy them.
5. Go to your registrar (where you bought the domain in step 1).
   Find the **Nameservers** setting (location varies by registrar —
   on Porkbun it's the domain's detail page; on Namecheap it's
   "Domain → Nameservers → Custom DNS").
6. **Replace** the registrar's default nameservers with the two from
   Cloudflare. Save.
7. Wait. Usually it's minutes; can take up to 24 hours. Cloudflare will
   email you when it's active. You can also poll yourself:

```
dig NS YOUR_DOMAIN +short
```

Once that returns the Cloudflare nameservers, you're good.

[cloudflare.com]: https://cloudflare.com

---

## Step 3 — Create a Cloudflare API token

This is the credential Beamd uses to manage DNS on your behalf.

1. In Cloudflare dashboard, click your profile picture (top right) →
   **My Profile**.
2. Left sidebar → **API Tokens** → **Create Token**.
3. Pick the **Edit zone DNS** template (click "Use template").
4. Under **Zone Resources** → choose **Include → Specific zone →
   YOUR_DOMAIN**.
5. Leave everything else as default. Click **Continue to summary** →
   **Create Token**.
6. **Cloudflare shows you the token ONCE.** Copy it to your password
   manager. This is `YOUR_CF_TOKEN`.

**Verify the token works** — from any machine with internet:

```
curl -s -H "Authorization: Bearer YOUR_CF_TOKEN" \
  https://api.cloudflare.com/client/v4/user/tokens/verify
```

You should see `"success":true` in the JSON.

---

## Step 4 — Rent a server

You need any Linux VM with a public IPv4. Cheapest reasonable options:

- **[Hetzner Cloud]** — CPX11, ~€4/month, EU regions.
- **[DigitalOcean]** — smallest Droplet, $4–6/month.
- **[Vultr]**, **[Linode]**, etc. — same ballpark.

[Hetzner Cloud]: https://hetzner.com/cloud
[DigitalOcean]: https://digitalocean.com
[Vultr]: https://vultr.com
[Linode]: https://linode.com

These instructions assume **Ubuntu 24.04 LTS**. Other distros work; only
the install commands change.

1. Create the smallest available instance with Ubuntu 24.04.
2. **Add your SSH public key** during creation (or use password auth and
   change it later — SSH keys recommended).
3. Note the **public IPv4 address** — this is `YOUR_SERVER_IP`.

**Verify:**

```
ssh root@YOUR_SERVER_IP
```

You should get a shell prompt.

---

## Step 5 — Point the domain at your server

In Cloudflare dashboard → `YOUR_DOMAIN` → **DNS** → **Records** →
**Add record**:

| Field | Value |
|---|---|
| **Type** | A |
| **Name** | `@` if `YOUR_DOMAIN` is the zone apex; the **subdomain label** if it's a subdomain (e.g. `beam` for `beam.mydomain.com`) |
| **IPv4 address** | `YOUR_SERVER_IP` |
| **Proxy status** | **DNS only** (gray cloud — *NOT* orange) |
| **TTL** | Auto |

> 🚨 **Proxy status must be "DNS only" (gray cloud).** If you set it to
> "Proxied" (orange cloud), Cloudflare terminates TLS itself and beamd's
> wildcard certs stop working.

Click **Save**.

**Verify:**

```
dig YOUR_DOMAIN +short
```

Returns `YOUR_SERVER_IP`.

---

## Step 6 — Install Docker on the server

SSH in (`ssh root@YOUR_SERVER_IP`), then:

```
curl -fsSL https://get.docker.com | sh
```

**Verify:**

```
docker --version
```

---

## Step 7 — Generate the config (`beamd init`)

Beamd has a one-shot interactive setup command. From inside the
running image — or after building the binary on the server (see
[Appendix A](#appendix-a--building-from-source) for the binary path) —
run:

```
docker run --rm -it \
  -v /etc/beamd:/etc/beamd \
  -v /var/lib/beamd:/var/lib/beamd \
  ghcr.io/dynamismlabs/beamd:latest init
```

It prompts for `base_domain`, `edge_ipv4`, `acme_email`, etc. (defaults
in brackets — just hit Enter to accept). It writes:

- `/etc/beamd/beamd.yaml`
- `/etc/beamd/tokens.json` (empty `{}` to start)
- `/var/lib/beamd/` (data dir for the cert cache)

> If you'd rather not be interactive, pass the values as flags and add
> `--non-interactive`:
>
> ```
> beamd init --non-interactive \
>   --base-domain YOUR_DOMAIN \
>   --edge-ipv4 YOUR_SERVER_IP \
>   --acme-email YOUR_EMAIL
> ```

Verify the files exist:

```
ls -la /etc/beamd
cat /etc/beamd/beamd.yaml
```

---

## Step 8 — Run Beamd

> **Image availability:** `ghcr.io/dynamismlabs/beamd:latest` is **not
> published yet** (coming soon). For now, build it on the server from a
> clone of the repo — this also gives you the `example/` files used below:
>
> ```
> git clone https://github.com/dynamismlabs/beamd /opt/beamd
> cd /opt/beamd && docker build -t ghcr.io/dynamismlabs/beamd:latest .
> ```
>
> (If you ran `beamd init` via the image in Step 7, build this first.)

Copy the bundled compose file + env template to `/etc/beamd/` from your
clone:

```
sudo cp /opt/beamd/example/docker-compose.yml /etc/beamd/docker-compose.yml
sudo cp /opt/beamd/example/.env.example       /etc/beamd/.env
```

Edit `/etc/beamd/.env` and paste in your Cloudflare API token from
step 3:

```
BEAMD_DNS_PROVIDER_CREDS=YOUR_CF_TOKEN
```

Optional: the yamux per-stream receive window defaults to 4 MiB. On very high
bandwidth-delay-product links you can raise it (bytes, `262144`–`16777216`) by
also setting `BEAMD_YAMUX_STREAM_WINDOW_BYTES`. It is process-wide (there is no
YAML equivalent); set the same variable for the agent on the developer side. A
present but empty or out-of-range value is a fatal startup error. A changed edge
value takes effect on the next edge restart; a changed agent value takes effect
after `beamd reload` (which restarts the background agent).

```
BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304
```

Part B makes QUIC available on UDP 443, but the permanent self-hosted default
keeps it disabled. Keep this in `/etc/beamd/.env` unless you explicitly opt
this edge into QUIC:

```text
BEAMD_DISABLE_QUIC=true
GOMEMLIMIT=1400MiB
```

Before an explicit QUIC pilot, prepare the host. Open both protocols in the
cloud firewall and host firewall (the exact cloud command varies):

```text
sudo ufw allow 443/tcp
sudo ufw allow 443/udp
```

Persist the Linux UDP receive/send ceilings in `/etc/sysctl.d/90-beamd-quic.conf`:

```text
net.core.rmem_max=7340032
net.core.wmem_max=7340032
```

Then apply them with `sudo sysctl --system`. These are host settings; setting
them only inside the container is ineffective. The compose file publishes both
`443:443/tcp` and `443:443/udp`.

Lock it down (the file holds your CF token — keep it private):

```
sudo chmod 600 /etc/beamd/.env
```

Start the service:

```
cd /etc/beamd
sudo docker compose up -d
```

**Verify:**

```
sudo docker compose logs beamd
```

You should see:

```
INFO ready version=… base_domain=YOUR_DOMAIN listen_https=:443 …
INFO edge listening addr=:443
```

Quick reachability check (TLS will fail with cert verification — that's
expected, we haven't issued a real cert for the apex yet):

```
curl -k https://YOUR_DOMAIN/healthz
```

Returns `{"status":"ok","version":"…"}`.

With QUIC still disabled, readiness must report only the TCP tunnel listener.
Before the pilot, run a TCP preflight from the matching developer binary:

```text
beamd check --transport tcp
```

For an explicit self-hosted pilot, set `BEAMD_DISABLE_QUIC=false`, restart the
edge, confirm its readiness/metrics show both listeners, then run
`beamd check --transport quic`. Set the developer environment to
`BEAMD_TRANSPORT=auto`, run `beamd reload`, and confirm `beamd status` reports
`transport: quic`. This opt-in does not change the default for this or other
self-hosted installations.

---

## Step 9 — Add yourself as a developer (`beamd add-developer`)

One command does everything: generates a token, appends it to
`tokens.json`, writes the DNS A record, and pre-issues your wildcard cert
from Let's Encrypt. By default it's **flat** — your tunnels live directly at
`<name>.YOUR_DOMAIN`:

```
sudo docker compose exec beamd \
  beamd add-developer --config /etc/beamd/beamd.yaml
```

It prints something like:

```
developer added:
  routing: flat — tunnels at <name>.YOUR_DOMAIN
  token:   4f2c8b7d1e09…  (64 hex chars)

Restart beamd to pick up the new token (the file is read at startup):
  docker restart beamd        # if running under Docker
  systemctl restart beamd     # if running as a systemd unit

Developer setup (their laptop):
  beamd login --server YOUR_DOMAIN --token <token above>
  beamd open 3001 --as api      # → https://api.YOUR_DOMAIN
```

> **Sharing one edge with developers who shouldn't collide on names?** Add
> `--slug YOUR_SLUG` to namespace your tunnels under
> `<name>.YOUR_SLUG.YOUR_DOMAIN` (it provisions `*.YOUR_SLUG.YOUR_DOMAIN`
> instead). Opt-in — skip it for a personal or trusting-team edge.

**Copy the token to your password manager now.** It's only printed
once. (You can always look it up in `/etc/beamd/tokens.json` later,
but the password-manager habit is better.)

Restart beamd so it picks up the new token:

```
sudo docker compose restart beamd
```

You should see in the logs:

```
INFO dns provisioned     slug="" …
INFO certs: ACME wildcard issued  slug="" issuance_count=1
```

(`slug=""` is the flat default. With `--slug YOUR_SLUG` you'd see your slug there.)

> If the ACME step fails: see [Troubleshooting](#troubleshooting).
> Common cause: wrong / under-scoped CF token.

**Verify DNS** (flat — for namespaced, use `api.YOUR_SLUG.YOUR_DOMAIN`):

```
dig api.YOUR_DOMAIN +short
```

Returns `YOUR_SERVER_IP` (matched by the `*.YOUR_DOMAIN` wildcard).

**Verify cert** (give it ~30 seconds after the provision finishes):

```
echo | openssl s_client -servername api.YOUR_DOMAIN \
  -connect YOUR_DOMAIN:443 2>/dev/null | \
  openssl x509 -noout -issuer -subject
```

Issuer should be Let's Encrypt:

```
issuer=C=US, O=Let's Encrypt, CN=R10
subject=CN=*.YOUR_DOMAIN
```

---

## Step 10 — Install the client on your laptop

The client and server are the **same binary** — `beamd serve` is the
edge; `beamd open` / `login` / `close` / `list` are the client. Install it
whichever way you like:

```
npm i -g @beamd/cli      # installs `beamd`; or `npx @beamd/cli <cmd>` ad-hoc
```

Or grab a prebuilt binary from the
[releases page](https://github.com/dynamismlabs/beamd/releases), or build
from source (needs Go 1.25.12+ or a newer supported Go release):

```
git clone https://github.com/dynamismlabs/beamd && cd beamd
make build          # → bin/beamd (also beam-testapp)
```

Use `./bin/beamd`, or copy it onto your `$PATH`.

**Verify:**

```
beamd version
```

---

## Step 11 — Log in from your laptop

```
beamd login --server YOUR_DOMAIN --token YOUR_TOKEN
```

Should print `logged in (YOUR_DOMAIN)`. This saves an **account** under
`~/.beamd/accounts/` (keyed by server) and marks it current. To stay logged
into more than one edge at once, just `beamd login` to each; every client
command takes `--server <edge>` to pick one (see the README's "Accounts"
section), and `beamd accounts` lists them.

---

## Step 12 — Expose a port (manual smoke test)

In one terminal, run any local web server. Easiest:

```
python3 -m http.server 3001
```

In another terminal:

```
beamd open 3001 --as hello
```

It prints one line:

```
https://hello.YOUR_DOMAIN
```

**Open that URL in a browser.** You should see Python's directory
listing, served with a real Let's Encrypt cert — no browser warnings,
no "your connection is not private" page.

That's Tier 1 working end-to-end. 🎉

### Run the bundled smoke test

For a more thorough sanity check, this repo ships:

- **`beam-testapp`** — a small test backend with routes that exercise
  header forwarding, POST bodies, response sizes, and slow responses.
- **`scripts/smoke-test.sh`** — drives `beam-testapp` through your
  real tunnel and reports pass/fail per check.

From the repo root, with `bin/beamd` already logged in:

```
make smoke-test
```

Expected output:

```
starting beam-testapp on :8765 ...
exposing :8765 as 'smoketest' …
tunnel URL: https://smoketest.YOUR_DOMAIN

checks:
  ✓ GET / serves the test app banner
  ✓ X-Forwarded-For header reaches the backend
  ✓ X-Forwarded-Proto is 'https'
  ✓ X-Forwarded-Host matches the tunnel hostname
  ✓ POST /echo returns the body verbatim
  ✓ GET /size?bytes=8192 returns 8192 bytes
  ✓ GET /sleep?ms=1000 succeeds

🎉 all smoke checks passed. your beam deployment is healthy.
```

If any check fails, see [docs/post-manual-testing.md](post-manual-testing.md)
for what to do next.

---

## Step 13 — Onboard a teammate

One command does everything — token + tokens.json + DNS + cert pre-warm. On a
shared edge, give each teammate a **slug** so your tunnel names can't collide
(this is exactly what namespacing is for):

```
sudo docker compose exec beamd \
  beamd add-developer --slug hopper --config /etc/beamd/beamd.yaml
```

(Drop `--slug` if you'd rather they share the flat namespace and just
coordinate names with you.) It prints `hopper`'s token. Send it to them via
Slack/Signal (private channels). Restart beamd so the new token is loaded:

```
sudo docker compose restart beamd
```

Then your teammate runs on their laptop:

```
beamd login --server YOUR_DOMAIN --token <theirs>
beamd open 3001 --as api
# → https://api.hopper.YOUR_DOMAIN
```

Restarting beamd briefly drops all clients, but they auto-reconnect
and re-register their tunnels — URLs stay stable.

---

## Troubleshooting

**ACME issuance fails with `unauthorized` or `forbidden`**
The Cloudflare token doesn't have `Zone:DNS:Edit` permission, or it's
not scoped to `YOUR_DOMAIN`. Recreate per step 3. Verify with the curl
in step 3.

**ACME issuance fails with `DNS lookup`-style errors**
DNS hasn't propagated yet. Wait a few minutes and retry
`docker exec beamd beamd provision-dev ...`.

**Browser shows "Your connection is not private" / NET::ERR_CERT_AUTHORITY_INVALID**
Either:
- Cert isn't issued yet (re-run `provision-dev`, watch `docker logs beamd`).
- The SNI you're hitting doesn't match the wildcard. The cert is one DNS
  label deep: `*.YOUR_DOMAIN` (flat) covers `api.YOUR_DOMAIN` but not
  `deep.nested.YOUR_DOMAIN`. (Namespaced edges are `*.YOUR_SLUG.YOUR_DOMAIN`,
  covering `api.YOUR_SLUG.YOUR_DOMAIN`.)

**`beamd open` hangs / "no client connected"**
Check `~/.beamd/agent.log` on your laptop. The background agent is what
holds the client→edge connection; if it can't reach the edge, `open`
blocks.

**`auto` selects TCP or QUIC is unstable**
First run `beamd check --transport quic` and verify UDP 443 in both the cloud
and host firewall. An undersized UDP socket buffer is reported in the agent
log. On macOS, test the OS default first; only when that diagnostic appears or
qualification fails for buffers, an operator may test
`sudo sysctl -w kern.ipc.maxsockbuf=8441037`.

To roll back only the laptop, set `BEAMD_TRANSPORT=tcp` and run
`beamd reload`. To roll back the edge globally, set
`BEAMD_DISABLE_QUIC=true` and restart it. Disabled mode must come up TCP-only
without binding UDP or reading QUIC key files.

**`Error: no route for host …`**
You're hitting an app/slug combination that hasn't been registered.
Run `beamd list` to see what's currently exposed.

**Need to revoke a developer**
Delete their entry from `tokens.json`, restart beamd. Their active
sessions are dropped immediately; they can't reconnect.

**Where are the logs?**

| What | Where |
|---|---|
| Edge server | `docker logs beamd` |
| Developer agent | `~/.beamd/agent.log` |

---

## Appendix A — Building from source

Until v0.1.0 is tagged + image published, build on the server:

```
# Install Git, then install Go 1.25.12+ from https://go.dev/doc/install.
# quic-go v0.60.0 requires Go 1.25; this repository pins the patched 1.25.12
# minimum and also builds with newer supported Go releases.
sudo apt update && sudo apt install -y git
go version

# Clone and build.
git clone https://github.com/dynamismlabs/beamd /opt/beam
cd /opt/beam
make build

# Install binaries.
sudo cp bin/beamd /usr/local/bin/
sudo setcap cap_net_bind_service=+ep /usr/local/bin/beamd

# Create a systemd unit.
sudo tee /etc/systemd/system/beamd.service >/dev/null <<EOF
[Unit]
Description=Beamd edge server
After=network.target

[Service]
Type=simple
User=root
Environment=BEAMD_DNS_PROVIDER_CREDS=YOUR_CF_TOKEN
# QUIC is permanently default-off for self-hosted edges; explicitly set false
# only when opting this edge into QUIC.
Environment=BEAMD_DISABLE_QUIC=true
# Optional: yamux per-stream receive window in bytes (default 4 MiB; 262144–16777216).
# Environment=BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304
ExecStart=/usr/local/bin/beamd serve --config /etc/beamd/beamd.yaml
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now beamd
sudo systemctl status beamd
```

`provision-dev` invocation in the binary path becomes (add `--slug YOUR_SLUG`
only if namespacing):

```
beamd provision-dev --config /etc/beamd/beamd.yaml
```

Logs:

```
journalctl -u beamd -f
```

---

## What's missing from this setup

- **Strangers can't sign themselves up.** To onboard someone, you generate
  their token + restart beamd. Self-serve signup requires the
  device-code login flow (see `TASKS.md` deferred section).
- **No automatic certificate-renewal monitoring.** certmagic renews certs
  before they expire; if it fails for some reason you won't be paged.
  Wire `/metrics` into Prometheus + alertmanager if this matters.
- **No abuse rate-limiting.** Body size is capped (32 MiB default), but
  there's no per-IP request rate limit. Put Cloudflare's free WAF rules
  in front, or a small fronting proxy (Caddy/nginx) with rate limits.
- **One beamd process.** No HA. If the VM dies, all tunnels drop until
  it's back. For personal use this is fine; for a team you may want a
  second VM + DNS failover.
