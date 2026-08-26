// The bundle assembler is its own module so that cedar-go — which it uses only
// to parse and split policy text at build time — does not land in the
// controller's vendor tree and ship inside the release binary. Same reasoning,
// and the same shape, as api/oapi-codegen.mod.
//
// Run it with GOWORK=off: Go refuses a nested module inside a workspace member
// otherwise.
module github.com/NVIDIA/nke/controller/hack/authz-bundle

go 1.26.5

require github.com/cedar-policy/cedar-go v1.8.0

require golang.org/x/exp v0.0.0-20251219203646-944ab1f22d93 // indirect
