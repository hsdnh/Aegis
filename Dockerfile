# Stage 1: Build
FROM golang:1.22-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags "-s -w" -o /ai-ops-agent ./cmd/agent/
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /ai-ops-agent-instrument ./cmd/instrument/
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /ai-ops-agent-init ./cmd/init/

# Stage 2: Runtime (minimal image)
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /ai-ops-agent /app/
COPY --from=builder /ai-ops-agent-instrument /app/
COPY --from=builder /ai-ops-agent-init /app/
COPY config.yaml /app/config.yaml

EXPOSE 9090
VOLUME ["/app/data"]

ENTRYPOINT ["/app/ai-ops-agent"]
CMD ["-config", "/app/config.yaml", "-dashboard", ":9090"]
