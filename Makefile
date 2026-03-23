.PHONY: build build-cli build-all run test test-cli lint docker clean

BINARY := mcp-wrangler
CLI_BINARY := mcp-sync

build:
	CGO_ENABLED=1 go build -o $(BINARY) ./cmd/mcp-wrangler

build-cli:
	CGO_ENABLED=0 go build -o $(CLI_BINARY) ./cmd/mcp-sync

build-all: build build-cli

run: build
	./$(BINARY) --config config.example.toml

test:
	CGO_ENABLED=1 go test ./...

test-cli:
	CGO_ENABLED=0 go test ./internal/cli/... ./cmd/mcp-sync/...

lint:
	go vet ./...

docker:
	docker compose build

clean:
	rm -f $(BINARY) $(CLI_BINARY)
	rm -rf dist/
