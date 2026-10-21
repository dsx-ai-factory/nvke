# local-site

The inputs of the local site: the operator flow end to end on a container node, bootstrapped by `nvkectl`. `hack/local-site.sh setup` (`make local-site-setup`) copies them into the operator's workspace and `deploy/nvke-deploy-local.sh` installs from there; [Run NVKE end to end on one machine](../../../docs/get-started/local-site.md) is the page.

| File | Used by |
|---|---|
| `stack-config.json` | `nvke-deploy.sh install`: nico-sim's endpoint on the node network's gateway and its seeded organization, and the controller and profile images the setup builds from the checkout and stages into the node, in place of the two `REPLACE_` values |
| `releases/{machineimage,clusterrelease,nodepoolrelease}-{amd64,arm64}.yaml` | `nvkectl create <kind> -f`: the `local-site-<arch>` release set the tenant nodes boot from |
| `tenant-{amd64,arm64}.yaml` | `nvkectl create cluster -f`: a one-control-plane, one-worker tenant on nico-sim's seeded identifiers |
| `nvke-api-clients.yaml` | `kubectl apply -f`, after replacing the two `REPLACE_` CIDRs: the networks admitted to the NVKE API |

The site spec is not here: `nvkectl generate sitespec` writes it per run from the node the setup started.
