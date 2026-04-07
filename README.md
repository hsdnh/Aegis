# AI Ops Agent

Production runtime intelligent monitoring and self-healing framework.

## Overview

AI Ops Agent is a pluggable monitoring framework that collects metrics and logs from production systems, evaluates alerting rules, and (optionally) uses AI to perform root cause analysis.

**Current status: Level 1 MVP** — monitoring + rule-based alerting, validated on mercari-hunter.

### Architecture

```
Data Collection (plugin-based, every 30 min)
├── Redis collector (queue length, memory, connections)
├── MySQL collector (connection count, slow queries, custom checks)
├── HTTP collector (API health, JSON path extraction)
└── Log collector (journalctl, docker, file-based)
        │
        ▼
Rule Engine (threshold alerts)
├── metric > threshold → alert
├── metric == 0 → alert (service down)
└── configurable severity levels
        │
        ▼
Alert Dispatch (pluggable)
├── Console (stdout)
├── Bark (iOS push)
└── Telegram
```

### Capability Levels

| Level | Description | Status |
|-------|-------------|--------|
| L1 | Monitoring + rule-based alerting | **Done** |
| L2 | AI-powered anomaly detection + root cause analysis | Planned |
| L3 | Code review suggestions on git push | Planned |
| L4 | Auto-fix with PR generation | Planned |

## Quick Start

```bash
# Build
go build -o ai-ops-agent ./cmd/agent/

# Run once (test mode)
./ai-ops-agent -config config.yaml -once

# Run as daemon (cron-based)
./ai-ops-agent -config config.yaml
```

## Configuration

See [config.yaml](config.yaml) for a full example targeting mercari-hunter.

Key environment variables:
- `REDIS_PASSWORD` — Redis auth
- `MYSQL_DSN` — MySQL connection string (e.g. `user:pass@tcp(host:3306)/db`)
- `API_USER` / `API_PASS` — HTTP basic auth
- `BARK_KEY` — Bark push notification key
- `TELEGRAM_BOT_TOKEN` — Telegram bot token

## Adding a Custom Collector

Implement the `Collector` interface:

```go
type Collector interface {
    Name() string
    Collect(ctx context.Context) (*types.CollectResult, error)
}
```

Register it in `cmd/agent/main.go`:

```go
registry.Register(myCollector)
```

## Project Structure

```
ai-ops-agent/
├── cmd/agent/main.go          # Entry point, wiring
├── internal/
│   ├── agent/agent.go         # Core monitoring loop orchestrator
│   ├── collector/
│   │   ├── collector.go       # Collector interface + registry
│   │   ├── redis.go           # Redis metrics collector
│   │   ├── mysql.go           # MySQL metrics collector
│   │   ├── http.go            # HTTP/API metrics collector
│   │   └── log.go             # Log collector (journalctl/docker/file)
│   ├── rule/engine.go         # Threshold-based rule engine
│   ├── alert/
│   │   ├── alert.go           # Alerter interface + manager
│   │   ├── bark.go            # Bark push notifications
│   │   ├── telegram.go        # Telegram notifications
│   │   └── console.go         # Console output
│   ├── config/config.go       # YAML config loader
│   ├── analyzer/              # (L2) AI analysis layer
│   ├── knowledge/             # (L2) Historical pattern storage
│   └── report/                # (L2) Report generation
├── pkg/types/types.go         # Shared types
├── config.yaml                # Sample config for mercari-hunter
└── go.mod
```

## License

MIT
