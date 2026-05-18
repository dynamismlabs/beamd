.PHONY: build test run-server clean tidy smoke-test

BIN_DIR := bin
VERSION ?= dev
LDFLAGS := -X main.Version=$(VERSION)

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/conduitd        ./cmd/conduitd
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/conduit         ./cmd/conduit
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/conduit-testapp ./cmd/conduit-testapp

smoke-test: build
	@PATH="$(PWD)/$(BIN_DIR):$$PATH" ./scripts/smoke-test.sh

test:
	go test ./...

run-server: build
	$(BIN_DIR)/conduitd serve --config example/conduitd.yaml

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)
