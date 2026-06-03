.PHONY: build test run-server clean tidy smoke-test npm-build publish-npm publish-binaries api-gen api-check

BIN_DIR := bin
VERSION ?= dev
LDFLAGS := -X main.Version=$(VERSION)

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/beamd        ./cmd/beamd
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/beam-testapp ./cmd/beam-testapp

smoke-test: build
	@PATH="$(PWD)/$(BIN_DIR):$$PATH" ./scripts/smoke-test.sh

test:
	go test ./...

# Regenerate the shared-contract Go types from internal/beamdapi/openapi.json
# (exported from the hosted web app). Commit the result.
api-gen:
	go generate ./internal/beamdapi/...

# CI guard: fail if the committed types are stale vs the spec.
api-check:
	./internal/beamdapi/check-drift.sh

run-server: build
	$(BIN_DIR)/beamd serve --config example/beamd.yaml

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

# --- Local publishing ---

# Build the npm packages into npm/build/ WITHOUT publishing (to inspect).
#   make npm-build VERSION=0.0.1
npm-build:
	@test "$(VERSION)" != "dev" || { echo "set VERSION, e.g. make npm-build VERSION=0.0.1"; exit 1; }
	node scripts/build-npm.mjs $(VERSION)

# Publish the npm package(s) from this machine. Run `npm login` once first.
#   make publish-npm VERSION=0.0.1
publish-npm:
	@test "$(VERSION)" != "dev" || { echo "set VERSION, e.g. make publish-npm VERSION=0.0.1"; exit 1; }
	@npm whoami >/dev/null 2>&1 || { echo "not logged in to npm — run 'npm login' first"; exit 1; }
	node scripts/build-npm.mjs $(VERSION) --publish

# Publish cross-platform binaries + the GHCR image via goreleaser.
# Prereqs: goreleaser installed, GITHUB_TOKEN exported, `docker login ghcr.io`
# done, and the version tag checked out (git tag vX.Y.Z && git push --tags).
publish-binaries:
	@command -v goreleaser >/dev/null 2>&1 || { echo "install goreleaser first: brew install goreleaser"; exit 1; }
	goreleaser release --clean
