# The embedded stack directory

This directory holds the Helm values and the glue manifests that `nvkectl`
installs the NVKE stack with. It holds no charts. It is not a chart
collection. The charts come from the artifact file; this directory only says
what each chart gets and which extra objects go next to it.

The `nvkectl` stack installer that reads this directory follows in a later
change. Until it lands, `deploy/README.md` describes the current install. The
installer embeds the directory into the binary with `go:embed`. `go:embed`
reaches only files inside the package tree, which is why the directory lives
inside the CLI module.

Everything here is plain Helm values and plain Kubernetes manifests. Anyone
can run them by hand with `helm` and `kubectl`.

## What is here

| Path | What it is | Passed to |
|---|---|---|
| `values/nktp.yaml` | The telemetry pipeline | the `nktp` chart |
| `values/postgresql.yaml` | PostgreSQL for the controller and Temporal | the Bitnami `postgresql` chart |
| `values/temporal.yaml` | Temporal | the `temporal` chart |
| `values/spire.yaml` | SPIRE, the OIDC discovery provider | the SPIFFE hardened `spire` chart |
| `values/controller.yaml` | The NVKE controller | the `nvke-controller` chart |
| `values/nico-prereqs.yaml` | NICo's database, seed only | the Bitnami `postgresql` chart |
| `values/nico.yaml` | NICo, seed only | the `nico` umbrella chart |
| `values/kind/<values file>.yaml` | Smaller sizing for the seed, one file per values file that needs it | the same chart, after its values file |
| `manifests/*.yaml` | Glue the charts do not create | `kubectl apply --server-side` |
| `manifests/nico/*.yaml` | Glue for NICo, seed only | `kubectl apply --server-side` |
| `manifests/*.yaml.tmpl` | Glue that needs an input value | rendered, then `kubectl apply --server-side` |

The `spire-crds` chart takes no values and has no file here.

## How nvkectl consumes it

The install order and the seed/site membership live in Go, not here. There
is no order file in this directory. A values file says what a chart gets. Go
says when, and whether a seed or a site gets it.

Every chart listed by the artifact file is installed as a Helm release. The
artifact file pins the chart. The values file here is passed to
`helm upgrade --install` with `--values`.

The seed is a kind cluster that runs NICo. It gets the same stack as a site,
NKTP included, so the monitors, the OIDC provider and the NetworkPolicy stay
on there. The kind layer changes sizing only. It is applied on top for the
seed cluster only: `values/kind/<name>.yaml` is passed as a second `--values`
after `values/<name>.yaml`. The file is named after the values file, not the
chart. The Bitnami `postgresql` chart has two, `kind/postgresql.yaml` and
`kind/nico-prereqs.yaml`. Helm merges maps key by key, so a kind file
restates only what changes.

Glue manifests are applied with `kubectl apply --server-side`. A second apply
of the same manifest does not conflict.

Credentials are Secrets that the command creates from the environment. The
values files reference them by name through `existingSecret` and the chart's
equivalent keys. No values file carries a password.

| Secret | Namespaces | Read by |
|---|---|---|
| `nvke-database` | `postgres`, `temporal`, `nvke-controller` | PostgreSQL and Temporal (`postgres-password`), the controller (`database-connection-string`, built from the same password) |
| `dgxc-tenant-credentials` | `nvke-controller` | the controller (`user`, `password`) |
| `spire-upstream-ca` | `spire-server` | SPIRE server (`tls.crt`, `tls.key`, `bundle.crt`), only when the site spec sets `spire.upstreamCA` |
| `nico-credentials` | `capnico-system` | CAPNICo (`endpoint`, `orgID`, `apiName`, `token`) |
| `nico-postgresql-auth` | `forge-system` | NICo's PostgreSQL (`postgres-password`, `password`), seed only |
| `nico.forge-pg-cluster.credentials` | `forge-system` | `nico-api` (`username`, `password`, `host`, `port`, `dbname`, `uri`; the URI carries `sslmode=disable` on kind), seed only |
| `nico-vault-approle-tokens`, `nico-vault-token` | `forge-system` | NICo's services (`VAULT_ROLE_ID`, `VAULT_SECRET_ID`; `VAULT_TOKEN`), seed only |
| `ssh-host-key` | `forge-system` | `nico-ssh-console-rs` (`ssh_host_ed25519_key`), seed only |

No wrapper charts exist. Each values file is written in the upstream chart's
own layout and goes straight to that chart.

## What is installed outside this directory

- cert-manager and Cluster API. `clusterctl init` installs both, with the
  kubeadm providers.
- A default StorageClass. Which provisioner is right depends on the hardware.
  PostgreSQL and SPIRE take a PVC, so the cluster must have a default.

## Rules that keep the directory reversible

- Values files are pure Helm values. No Go templating, no placeholders that a
  render step fills in.
- Input-derived values go into a generated overlay at render time. The site
  spec and the stack config supply them. The overlay is passed as a later
  `--values`, so it wins. Each values file lists in its header what the
  overlay supplies.
- Glue manifests are plain Kubernetes manifests. The few that need an input
  value are templates, and the next section lists every one of them.
- Nothing here is derived from a release name. Service hostnames are pinned
  with `fullnameOverride`, so a values file reads the same whatever Go calls
  the release.

## Templated manifests

| File | Input |
|---|---|
| `manifests/nktp-cluster.yaml.tmpl` | `.ClusterName`, the site's name from the site spec |
| `manifests/capnico.yaml.tmpl` | `.CAPNICoImage`, the `capnico-controller` reference from the artifact file. The rest of the file is CAPNICo's released `infrastructure-components.yaml`, generated from `deploy/nico/capnico/capnico.yaml` by `make -C cli capnico-manifest`; do not edit it here. |
| `manifests/nvke-api-clients.yaml.tmpl` | `.ClientNetworks`, the CIDRs from the site spec that may reach the API: the host that runs `nvkectl` and the tenant node network. The controller chart's NetworkPolicy admits pods only, so without this policy nothing outside the cluster reaches the API. |

Every other file under `manifests/` is plain YAML.

## Manifests with install rules

- `manifests/oidc-upstream-ca-certificate.yaml` is applied on a first install
  only. The command skips it once the Secret `oidc-provider-upstream-ca`
  exists. A changed Certificate spec re-issues the CA with a new key, and
  every tenant trusts the old one.
- `manifests/tpm-pubhashes.yaml` is applied empty. The controller fills it
  under its own field manager. A server-side re-apply of the empty file
  leaves the controller's keys in place.

## Running it by hand

```bash
kubectl apply --server-side -f manifests/namespaces.yaml
# Render manifests/nktp-cluster.yaml.tmpl with the site's name first.
kubectl apply --server-side -f <rendered nktp-cluster.yaml>
helm upgrade --install nktp <chart> --namespace nktp \
  --values values/nktp.yaml
helm upgrade --install postgres <chart> --namespace postgres \
  --values values/postgresql.yaml --values values/kind/postgresql.yaml
```

Create the Secrets from the table above first. Pass the generated overlay as
the last `--values`.
