// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	_ "embed"
)

//go:embed openapi.yaml
var OpenapiYAML string

// The oapi-codegen generator is pinned in its own module file
// (oapi-codegen.mod / oapi-codegen.sum) so `go generate` is reproducible and
// does not depend on whatever version happens to be on PATH. It lives in a
// separate module because generator v2.2.0 requires an older kin-openapi
// (v0.124.0) than the runtime api module (v0.133.0); `-modfile` isolates that
// dependency graph without downgrading the runtime module. To bump the
// generator, run `go get -modfile=oapi-codegen.mod
// github.com/deepmap/oapi-codegen/v2/cmd/oapi-codegen@<ver>` and regenerate.
//
// GOWORK=off is required, not incidental. This module is a member of the
// repository's Go workspace, and Go refuses `-modfile` in workspace mode:
// "go: -modfile cannot be used in workspace mode". Without it `go generate`
// fails here however it is invoked, so the setting belongs on the directives
// rather than in one caller. It only affects the generator's own module
// resolution; the generated output is identical.
//go:generate env GOWORK=off go run -modfile=oapi-codegen.mod github.com/deepmap/oapi-codegen/v2/cmd/oapi-codegen --config=config-models.yaml ./openapi.yaml
//go:generate env GOWORK=off go run -modfile=oapi-codegen.mod github.com/deepmap/oapi-codegen/v2/cmd/oapi-codegen --config=config-server.yaml ./openapi.yaml
//go:generate env GOWORK=off go run -modfile=oapi-codegen.mod github.com/deepmap/oapi-codegen/v2/cmd/oapi-codegen --config=config-client.yaml ./openapi.yaml
//go:generate go run ./patch-optional-bodies.go
//go:generate go run ./prepend-license.go

// The generator does not emit gofumpt-clean code, so the output is formatted
// here to match the `extra-rules` gofumpt config in .golangci.yml. Pinned via
// `go run` (not PATH) so CI images without gofumpt installed still work; keep
// the version in sync with the gofumpt that golangci-lint bundles and with the
// `fmt` target in the root Makefile.
//go:generate go run mvdan.cc/gofumpt@v0.11.0 -w -extra models.gen.go server.gen.go client.gen.go
