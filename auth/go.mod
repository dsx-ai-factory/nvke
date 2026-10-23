module github.com/dsx-ai-factory/nvke/auth

go 1.27.3

require (
	github.com/dsx-ai-factory/nvke/controller/api v0.0.0-00010101000000-000000000000
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/spiffe/go-spiffe/v2 v2.9.0
	github.com/stretchr/testify v1.12.1
	golang.org/x/sync v0.24.0
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/apimachinery v0.37.1
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730
	sigs.k8s.io/yaml v1.6.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.1 // indirect
	github.com/getkin/kin-openapi v0.151.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-openapi/jsonpointer v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	k8s.io/kube-openapi v0.0.0-20260721132016-d427ff9ee9ad // indirect
	k8s.io/utils v0.0.0-20260626114624-be93311217bd // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
	sigs.k8s.io/structured-merge-diff/v6 v6.4.2 // indirect
)

replace github.com/dsx-ai-factory/nvke/controller/api => ../controller/api
