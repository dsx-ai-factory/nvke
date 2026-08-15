# Controller upstream sync

During the open-source migration, **GitLab remains the writable source of truth** for controller code until a GitHub CI/release pipeline replaces it. This tree is a flat snapshot (no rewritten GitLab history). Pull updates here with a deliberate sync, not `git merge`. Syncing is going to be painful so our plan is to try and do it only one more time just before cutover (assuming the pipeline can be rewritten on GitHub in a reasonable amount of time).

## Pin

| Field | Value |
| --- | --- |
| Upstream | https://gitlab-master.nvidia.com/nke/cloud-workflow-engine |
| Branch | `main` |
| Last imported revision | `fa91e66f` (`v0.0.650-7-gfa91e66f`) |
| Destination | `controller/` in https://github.com/NVIDIA/nke |

## Transforms applied after upstream import

1. Drop GitLab CI leftovers: `.gitlab-ci.yml`, `.gitlab/`.
2. Rewrite the self-module path everywhere it appears (Go imports, `go.mod`, scripts, docs as needed):

   `gitlab-master.nvidia.com/nke/cloud-workflow-engine` → `github.com/NVIDIA/nke/controller`
3. Update goimports local prefixes so lint stays green after the module rewrite (`.golangci.yml` formatters/linters settings, and `Makefile` `fmt`):

   keep `gitlab-master.nvidia.com`, and add `github.com/NVIDIA/nke/controller`
4. Preserve the public dependency migrations made on GitHub:

   - `gitlab-master.nvidia.com/nke/node-agent` → `github.com/NVIDIA/nke/agent`
   - `gitlab-master.nvidia.com/nke/cluster-api-provider-nico` → `github.com/NVIDIA/cluster-api-provider-nico`
   - `gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go` → `github.com/NVIDIA/ncx-infra-controller-rest`
5. Preserve the GitHub-owned monorepo integration: root Go workspace and vendor
   wiring, public container build inputs, license packaging, and image/chart
   publishing targets.

## Sync procedure after GitHub divergence

Once `controller/` has GitHub-only changes, do not replace it with an upstream
archive. Use the last imported GitLab revision as the merge base, current
`controller/` as the GitHub side, and transformed GitLab `main` as the upstream
side. Resolve the resulting three-way merge, then update the root Go workspace
and vendor tree for any added or removed modules.
