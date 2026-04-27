FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# -tags sqlite_fts5: enable FTS5 in mattn/go-sqlite3 (the bundled sqlite
# build does NOT include FTS5 by default; required for memory_messages_fts).
RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -o /arc-relay ./cmd/arc-relay

FROM alpine:3.21

RUN apk add --no-cache ca-certificates sqlite-libs git

COPY --from=builder /arc-relay /usr/local/bin/arc-relay

RUN mkdir -p /data

# Default DB path inside the container - matches the /data volume mount
ENV ARC_RELAY_DB_PATH=/data/arc-relay.db
ENV ARC_RELAY_MEMORY_DB_PATH=/data/memory.db

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/arc-relay"]
