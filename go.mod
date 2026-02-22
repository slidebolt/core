module core

go 1.24.0

// NOTE: Standing by for GitHub repositories to be finalized.
// Standalone builds will fail until these are available.
// For local development, use the go.work file in this directory.

// require (
// 	github.com/slidebolt/plugin-framework v0.0.0
// 	github.com/slidebolt/plugin-loader v0.0.0
// 	github.com/slidebolt/plugin-sdk v0.0.0
// )

require (
	github.com/gorilla/websocket v1.5.3
	github.com/nats-io/nats-server/v2 v2.12.4
	github.com/nats-io/nats.go v1.48.0
)
