# local-site

The inputs of the local site: the operator flow end to end on container nodes, bootstrapped by `nvkectl`. [Run NVKE end to end on one machine](../../../docs/get-started/local-site.md) is the page that uses them; `cli/e2e/bootstrap/run.sh` runs the same steps with `NVKECTL_E2E_INSTALL_STACK=1`.

| File | Used by |
|---|---|
| `stack-config.json` | `check-site.sh` and `nvke-deploy.sh install`: nico-sim's endpoint on the node network's gateway and its seeded organization |
| `releases/{machineimage,clusterrelease,nodepoolrelease}-{amd64,arm64}.yaml` | `nvkectl create <kind> -f`: the `local-site-<arch>` release set the tenant nodes boot from |
| `tenant-{amd64,arm64}.yaml` | `nvkectl create cluster -f`: a one-control-plane, one-worker tenant on nico-sim's seeded identifiers |
| `nvke-api-clients.yaml` | `kubectl apply -f`, after replacing the two `REPLACE_` CIDRs: the networks admitted to the NVKE API |

The site spec and the stack artifacts are not here: `nvkectl generate sitespec` writes the spec per run from the nodes, and `make local-stack-artifacts` writes `dist/local-site/stack-artifacts.json` from the checkout.
