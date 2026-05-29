# Releasing beamd

A practical runbook for cutting a release. The **local path** is the
default (no GitHub config, nothing extra to install for the common case);
**CI** is an optional hands-off alternative. Both produce the same result.

---

## What a release publishes

| Channel | Install command | Mechanism | Needs |
|---|---|---|---|
| **npm** | `npm i -g beamd` / `npx beamd` | `make publish-npm` (or CI) | `npm login` (or `NPM_TOKEN`) |
| **Go** | `go install github.com/dynamismlabs/beamd/cmd/beamd@vX.Y.Z` | **automatic from the git tag** | nothing — just push the tag |
| **Binaries** | download from Releases | goreleaser | goreleaser + `GITHUB_TOKEN` |
| **Docker** | `docker pull ghcr.io/dynamismlabs/beamd:X.Y.Z` | goreleaser | goreleaser + `docker login ghcr.io` |

The single `beamd` binary is both the edge (`beamd serve`) and the client
(`beamd open`/`run`/`close`/…). The npm package ships a tiny JS shim plus a
per-platform binary (`beamd-{darwin,linux}-{arm64,x64}`) installed as
`optionalDependencies`, so `npm i beamd` pulls only the host's binary.

---

## Versioning

- Semver, tags shaped `vX.Y.Z` (e.g. `v0.0.1`). The leading `v` is in the
  git tag; npm gets the bare `X.Y.Z`.
- **Never reuse a version.** npm refuses to republish an existing version;
  if you need a fix, bump the patch.
- Start: `0.0.1`.

---

## One-time setup

### npm (required for the npm channel)
```
npm login          # browser auth, once per machine
```
The **first** publish claims `beamd` and the four `beamd-*` names under the
logged-in account — make sure it's the account/org you want to own them.

### Binaries + Docker image (optional — only if you want them)
```
brew install goreleaser
export GITHUB_TOKEN=<a GitHub PAT with 'repo' scope>      # for the GitHub release
echo <a GitHub PAT with 'write:packages'> | docker login ghcr.io -u <you> --password-stdin
```

> npm + `go install` need **none** of this — you already have node + go.

---

## Release — local path (recommended)

From a clean working tree on `main`, with tests green (`make test`):

```
# 1. (first time only) log in to npm
npm login

# 2. publish the npm package(s) — builds all 4 platforms, then publishes
make publish-npm VERSION=0.0.1

# 3. tag + push — this is what makes `go install …@v0.0.1` work
git tag v0.0.1
git push origin v0.0.1

# 4. (optional) binaries + Docker image, if goreleaser is set up above
make publish-binaries
```

`make publish-binaries` runs `goreleaser release --clean`, which builds all
platforms, creates a **draft** GitHub release with the binaries +
checksums, and pushes `ghcr.io/dynamismlabs/beamd:0.0.1` + `:latest`.
Publish the draft from the GitHub Releases UI when you're happy with it.

> Inspect before publishing: `make npm-build VERSION=0.0.1` builds the
> packages into `npm/build/` without publishing. `cd npm/build/beamd &&
> npm publish --dry-run` shows exactly what would ship.

---

## Release — via CI (alternative, hands-off)

The workflow (`.github/workflows/release.yml`) is **manual** so it never
collides with local publishing.

1. One-time: add repo secret **`NPM_TOKEN`** (npm → Access Tokens →
   Automation). GHCR uses the built-in token.
2. Push the tag: `git tag v0.0.1 && git push origin v0.0.1`.
3. GitHub → **Actions → release → Run workflow**, and in the ref selector
   pick the **`v0.0.1` tag**. It runs goreleaser + npm publish from that tag.

To make CI fire automatically on every tag instead, change the trigger in
the workflow back to `on: { push: { tags: ["v*"] } }` — but then **don't
also publish locally** (you'd double-publish the same version).

---

## Verify

```
npx beamd@0.0.1 version                 # → 0.0.1
npm view beamd version                  # → 0.0.1
go install github.com/dynamismlabs/beamd/cmd/beamd@v0.0.1 && beamd version
docker pull ghcr.io/dynamismlabs/beamd:0.0.1
```

The GHCR image is **private by default** — make the package public in the
org's package settings for anonymous `docker pull`.

---

## Gotchas

- **Don't publish the same version both locally and via CI** — npm rejects
  the duplicate and goreleaser hits the existing release. Pick one path per
  version. (CI is manual by default specifically to avoid this.)
- **npm can't republish a version.** Botched a publish? Bump the patch.
- **`go install` needs nothing but the tag** on a public repo — there is no
  Go "publish" step; the module proxy serves it on first request.
- Keep the tag and the published versions identical (`vX.Y.Z` tag ↔
  `X.Y.Z` on npm / the image) so `go install …@vX.Y.Z` lines up.

---

## Fixing a bad release

- **npm:** within 72h and if nothing depends on it, `npm unpublish
  beamd@X.Y.Z` (and the `beamd-*` platform packages). Otherwise
  `npm deprecate beamd@X.Y.Z "use X.Y.Z+1"` and publish a fixed bump.
- **git tag:** `git tag -d vX.Y.Z && git push origin :refs/tags/vX.Y.Z` to
  remove it (only safe before anyone has fetched it — once it's on the Go
  proxy it's effectively permanent, so prefer a bump).
- **GitHub release:** delete the draft/release in the UI.
- **Rule of thumb:** don't fight a published version — ship the next patch.

---

## Future: Homebrew & other package managers

Not set up yet. goreleaser can add them via `.goreleaser.yaml` blocks:

- **Homebrew** (`brews:`) — needs a separate `dynamismlabs/homebrew-tap`
  repo + a PAT (`HOMEBREW_TAP_TOKEN`); then `brew install dynamismlabs/tap/beamd`.
- **.deb / .rpm / .apk** (`nfpms:`) — Linux packages attached to the release;
  full `apt`/`yum` repos need a host (Cloudsmith/packagecloud).
- **AUR** (`aurs:`), **Snap** (`snapcrafts:`) — niche; each needs its own credential.
- **Scoop / winget / Chocolatey** — Windows only; **N/A** until beamd has a
  Windows build (it's unix-only today: unix-domain agent socket + process
  groups in `run`).
