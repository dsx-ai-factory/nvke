module github.com/NVIDIA/nke/os-image

go 1.26.5

require (
	github.com/NVIDIA/nke/agent v0.0.0-00010101000000-000000000000
	github.com/spiffe/go-spiffe/v2 v2.8.1
	github.com/stretchr/testify v1.11.1
	google.golang.org/grpc v1.83.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260807164820-c8921c73eeea // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/NVIDIA/nke/agent => ../agent
