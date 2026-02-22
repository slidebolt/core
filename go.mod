module core

go 1.24.0

replace plugin-sdk => ../plugin-sdk

replace plugin-framework => ../plugin-framework

replace plugin-system => ../plugin-system

replace plugin-automation => ../plugin-automation

replace plugin-loader => ../plugin-loader

require (
	github.com/gorilla/websocket v1.5.3
	github.com/nats-io/nats-server/v2 v2.12.4
	github.com/nats-io/nats.go v1.48.0
	plugin-framework v0.0.0
	plugin-loader v0.0.0
	plugin-sdk v0.0.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.5.0-default-no-op // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/klauspost/compress v1.18.3 // indirect
	github.com/minio/highwayhash v1.0.4-0.20251030100505-070ab1a87a76 // indirect
	github.com/nats-io/jwt/v2 v2.8.0 // indirect
	github.com/nats-io/nkeys v0.4.12 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	golang.org/x/crypto v0.47.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/time v0.14.0 // indirect
)
