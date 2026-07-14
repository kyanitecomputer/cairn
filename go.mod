module src.kyanite.computer/cairn

go 1.27

// Note: github.com/usbarmory/tamago is intentionally not listed here. It is
// supplied by the workspace (go.work -> ../tamago), the fork carrying ASPEED
// AST2700 arm64 SoC/board support, until it lands upstream. Dagger builds
// provide it via an in-container go.work. Listing it with a placeholder version
// breaks module-graph loading.
require (
	github.com/nats-io/nats-server/v2 v2.12.1
	go.opentelemetry.io/contrib/bridges/otelslog v0.17.0
	go.opentelemetry.io/otel/log v0.18.0
	go.opentelemetry.io/otel/sdk/log v0.18.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0-default-no-op // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/jwt/v2 v2.8.2 // indirect
	github.com/nats-io/nats.go v1.51.0 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.42.0 // indirect
	go.opentelemetry.io/otel/metric v1.42.0 // indirect
	go.opentelemetry.io/otel/sdk v1.42.0 // indirect
	go.opentelemetry.io/otel/trace v1.42.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
