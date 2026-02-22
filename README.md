# IoT Core

The central hub of the IoT Dashboard ecosystem. Core handles message routing, provides a WebSocket API for the UI, and manages plugin sidecars.

## Responsibilities

- **NATS Server**: Starts an embedded NATS server for high-performance messaging between components.
- **Plugin Loader**: Orchestrates the discovery and execution of plugin sidecars.
- **Unified API**: Provides a central HTTP/WebSocket interface for the frontend to interact with all devices and entities.
- **MCP Integration**: Implements a basic Model Context Protocol (MCP) server for tool discovery.

## Configuration

Core is configured via environment variables. Copy `.env.example` to `.env` to customize your setup:

| Variable | Description | Default |
|----------|-------------|---------|
| `CORE_API_ADDR` | Listen address for the HTTP/WS API | `127.0.0.1:49800` |
| `NATS_ADDR` | Address for the embedded NATS server | `127.0.0.1:4222` |
| `PLUGINS_DIR` | Directory to scan for plugin binaries | `./plugins` |

## Getting Started

### Prerequisites

- Go (v1.24+)
- NATS (Embedded, no separate installation required)

### Building

```bash
go build -o core ./cmd/main.go
```

### Testing

```bash
go test ./...
```

## Internal Architecture

- `cmd/main.go`: Application entry point and server orchestration.
- `internal/api/`: Implementation of the HTTP and WebSocket API handlers.
- `internal/mcp/`: MCP protocol implementation for tool calling.

## License

Refer to the root project license.
