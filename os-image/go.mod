module github.com/dsx-ai-factory/nvke/os-image

go 1.27.2

require (
	github.com/dsx-ai-factory/nvke/agent v0.0.0-00010101000000-000000000000
	github.com/spiffe/go-spiffe/v2 v2.9.0
	github.com/stretchr/testify v1.12.1
	google.golang.org/grpc v1.84.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/go-jose/go-jose/v4 v4.1.5 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/dsx-ai-factory/nvke/agent => ../agent
