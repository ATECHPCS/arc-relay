.PHONY: build run test lint docker clean

BINARY := mcp-wrangler

build:
	CGO_ENABLED=1 go build -o $(BINARY) ./cmd/mcp-wrangler

run: build
	./$(BINARY) --config config.example.toml

test:
	CGO_ENABLED=1 go test ./...

lint:
	go vet ./...

docker:
	docker compose build

clean:
	rm -f $(BINARY)
	rm -rf dist/
