# Controller upstream sync

During the open-source migration, **GitLab remains the writable source of truth** for controller code until a GitHub CI/release pipeline replaces it. This tree is a flat snapshot (no rewritten GitLab history). Pull updates here with a deliberate sync, not `git merge`. Syncing is going to be painful so our plan is to try and do it only one more time just before cutover (assuming the pipeline can be rewritten on GitHub in a reasonable amount of time).

## Pin

| Field | Value |
| --- | --- |
| Upstream | https://gitlab-master.nvidia.com/nke/cloud-workflow-engine |
| Branch | `main` |
| Last imported revision | `d1f63d61` (`v0.0.616`) |
| Destination | `controller/` in https://github.com/NVIDIA/nke |

## Transforms applied after upstream import

1. Drop GitLab CI leftovers: `.gitlab-ci.yml`, `.gitlab/`.
2. Rewrite the self-module path everywhere it appears (Go imports, `go.mod`, scripts, docs as needed):

   `gitlab-master.nvidia.com/nke/cloud-workflow-engine` → `github.com/NVIDIA/nke/controller`
3. Update goimports local prefixes so lint stays green after the module rewrite (`.golangci.yml` formatters/linters settings, and `Makefile` `fmt`):

   keep `gitlab-master.nvidia.com`, and add `github.com/NVIDIA/nke/controller`
