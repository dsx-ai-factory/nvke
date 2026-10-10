# CAPNICo CRD fixture

The four `NicoCluster`, `NicoClusterTemplate`, `NicoMachine` and
`NicoMachineTemplate` CustomResourceDefinitions, copied verbatim from
`dsx-ai-factory/cluster-api-provider-nico` at the version this repository pins in
`controller/go.mod` (currently **`v0.0.46-rc.4`**).

## Why they live here

The gofail E2E suite installs these CRDs into its envtest control plane
(`NKE_CONTROLLER_GOFAIL_E2E_CAPNICO_CRDS`). They are not in the vendored module — `go mod
vendor` keeps only `.go` files — so the workflow used to `git checkout` the
provider repository, which was private then and needed `NKE_INTERNAL_RO_TOKEN`.
GitHub does not pass secrets to `pull_request` runs from forks, so `gofail`
failed for every fork contributor. These committed copies remove that
dependency: the suite reads them straight from the tree, with no token and no
checkout. That still holds now the provider is public, because it keeps the
suite off the network entirely.

CRDs are declarative API schemas, not code or secrets, and ship in every
provider release. Committing them as a fixture follows the same call as the
nico-vms profile fixtures next door.

## Keeping them current

`gofail-e2e.yml` verifies these files against the pinned provider on every push
to `main`, on the schedule and on `workflow_dispatch` — the runs that have the
token — and fails on any drift. Fork `pull_request` runs skip that check and use
the fixture as-is.

To refresh after a `cluster-api-provider-nico` bump in `go.mod`:

```sh
ver="$(cd controller && go list -m -f '{{.Version}}' github.com/dsx-ai-factory/cluster-api-provider-nico)"
for f in nicoclusters nicoclustertemplates nicomachines nicomachinetemplates; do
  gh api "repos/dsx-ai-factory/cluster-api-provider-nico/contents/config/crd/bases/infrastructure.cluster.x-k8s.io_${f}.yaml?ref=${ver}" \
    --jq '.content' | base64 -d \
    > "controller/test/fixtures/capnico-crds/infrastructure.cluster.x-k8s.io_${f}.yaml"
done
```
