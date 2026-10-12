---
name: regenerate
description: Regenerate generated artifacts (OpenAPI Go code, gRPC protos, CAPI deepcopy/CRD/RBAC, the controller chart's release CRDs, third-party notices, go.work) after editing their sources. Use whenever a change touches openapi.yaml, a .proto file, CAPI types/markers, the release CRD types, dependencies, or when a *-check CI gate reports drift.
---

# Regenerate, never hand-edit

Generated files are listed in the root [AGENTS.md](../../../AGENTS.md)
directory table. Change the source, run the matching generator, and commit
source + output together.

| You changed | Run | Commit together |
|---|---|---|
| `controller/api/openapi.yaml` | `make generate` in `controller/` | spec + `api/*.gen.go` |
| `agent/pkg/agent/proto/node_agent.proto` | `make protos` in `agent/` | `.proto` + `*.pb.go` |
| CAPI types/markers in `cluster-api-provider-provisioned/` | `make generate manifests` there | types + deepcopy, CRDs, `config/rbac/role.yaml` |
| `MachineImage`, `ClusterRelease` or `NodePoolRelease` types/markers in `management-engine/apis/v1/` | `make -C management-engine manifests`, then `make chart-crds` at the root | types + `config/crd/bases` + the chart's `files/crds.yaml` |
| `controller/deploy/release-crds/kustomization.yaml` | `make chart-crds` at the root | overlay + `files/crds.yaml` |
| any module `go.mod`/`go.sum` | `make work-sync`, then `make notices`, at the root | `go.work` + `go.work.sum` + every changed `THIRD_PARTY_NOTICES.md` |

Rules that trip people up:

- **Use `make generate`, never invoke `oapi-codegen` directly.** The generator
  version is pinned in `api/oapi-codegen.mod` and run through `go generate`,
  so the output does not depend on whatever `oapi-codegen` is on `PATH`.
- **Prefer Flox when available.** If `flox` is on `PATH` and you are not
  already inside the environment (`FLOX_ENV` is unset), run generators as
  `flox activate -- make <target>` so the rest of the toolchain matches CI.
  Flox is optional — plain `make` works without it. If you do activate it,
  match the CLI version in `.flox-cli-version`.
- Drift gates in CI: `make check-protos` (agent), `make notices-check` (root),
  `make generate-check` (provisioned provider), `make chart-crds-check` (the
  controller chart's release CRDs), `go-work-check` (workspace).
  If one fails, regenerate and commit the result — do not revert the output.
- `controller/deploy/charts/` READMEs are helm-docs output of `values.yaml`
  comments; edit the comment, not just the README.
