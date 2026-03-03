FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /mcp-wrangler ./cmd/mcp-wrangler

FROM alpine:3.21

RUN apk add --no-cache ca-certificates sqlite-libs

COPY --from=builder /mcp-wrangler /usr/local/bin/mcp-wrangler

RUN mkdir -p /data

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/mcp-wrangler"]
