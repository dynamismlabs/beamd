.PHONY: build test run-server clean tidy smoke-test

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

run-server: build
	$(BIN_DIR)/beamd serve --config example/beamd.yaml

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)
