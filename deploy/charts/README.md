# Helm charts

Five charts, installed as separate releases **in order**, each waited on
before the next.

| # | Chart | Installs | Namespace |
|---|---|---|---|
| 0 | [`storage/`](storage/) | A default StorageClass, for a cluster with none | `nvke-storage` |
| 1 | [`spire/`](spire/) | SPIRE, the OIDC upstream CA chain and the two NodePorts | `spire-server` |
| 2 | [`nvke/`](nvke/) | The NVKE controller, with the PostgreSQL and Temporal it runs on | `nvke-system` |
| 3 | [`nico-prereqs/`](nico-prereqs/) | NICo's namespace, database, credentials and issuer | `forge-system` |
| 4 | [`nico/`](nico/) | NICo itself | `forge-system` |

**The order is load-bearing, and so is waiting between steps.** It is not a
style preference:

- SPIRE before NVKE, because the controller blocks on "no identity issued"
  until SPIRE has issued its SVID.
- `nico-prereqs` before `nico`, because the NICo chart runs its config
  ConfigMap and its schema migration Job as `pre-install,pre-upgrade` hooks,
  and helm applies hooks **before every regular manifest in their own
  release**. The namespace, the database and the credentials Secret therefore
  cannot be in the NICo release — they would be created after the hooks that
  read them.
- The StorageClass before anything that takes a PVC, which is steps 2 and 3.

There is deliberately no umbrella chart. One release cannot sequence any of
this: see [below](#why-there-is-no-umbrella-chart).

**CAPNICo is not here.** It is a Cluster API infrastructure provider, so
`clusterctl init --infrastructure nico` installs it — the same command that
brings Cluster API core and cert-manager. See [below](#cluster-api-cert-manager-and-capnico).

These charts are an alternative to the [deploy path](../nico/), not a
replacement for it. The deploy path renders per-cluster values and credentials
from a site spec, pins every artifact to a release digest, and imports images
into the node's containerd before it applies anything. A Helm chart does none
of that. Use the deploy path to install a real cluster from a release; use
these charts for a development cluster, or where a GitOps tool already drives
Helm.

## Before you start

| Prerequisite | Why |
|---|---|
| cert-manager | Every chart here creates `Certificate` and `Issuer` objects, so their CRDs have to exist first. **`clusterctl init` installs it** — see below |
| CAPNICo | Only for driving NICo hardware. **`clusterctl init --infrastructure nico` installs it** — see below |
| A default StorageClass | PostgreSQL and SPIRE both take a PVC. Without a default they sit `Pending` forever. kind ships the rancher local-path provisioner, and a cluster with real storage brings its own CSI driver. For a bare cluster that has neither, [`storage/`](storage/) supplies one — set `nvke-storage.enabled=true` |
| Cluster API, through `clusterctl init` | For driving real clusters. The NVKE controller starts and answers without it, and logs that `cluster.x-k8s.io` kinds are missing |
| A `nico` chart repository | Only for [`nico/`](nico/). NICo is published separately from NVKE, so the chart names no registry: `helm repo add nico <your NICo chart repository>` |

### Cluster API, cert-manager and CAPNICo

One command brings all three. Do not install any of them separately: a second
installation does not give you two cert-managers or two CAPNICos — it gives
you two releases claiming the same cluster-scoped objects.

```bash
# v1.13.6 is CAPI_VERSION in make/tool-versions.mk. Read it from there rather
# than from this line, which will go stale.
clusterctl init \
  --core cluster-api:v1.13.6 \
  --bootstrap kubeadm:v1.13.6 \
  --control-plane kubeadm:v1.13.6 \
  --infrastructure nico:<version> \
  --config ./clusterctl.yaml \
  --wait-providers
```

`--infrastructure nico` needs the provider in your `clusterctl` configuration,
because it is not one of the providers `clusterctl` knows by default. Every
CAPNICo release publishes the `infrastructure-components.yaml` and
`metadata.yaml` it points at:

```yaml
# clusterctl.yaml
providers:
  - name: nico
    url: https://github.com/dsx-ai-factory/cluster-api-provider-nico/releases/download/<version>/infrastructure-components.yaml
    type: InfrastructureProvider
```

**Pass `--config` explicitly.** `clusterctl` resolves its default
configuration path through the OS convention, so
`~/.config/cluster-api/clusterctl.yaml` is not read on macOS. The failure is
unhelpful: `failed to get configuration for the InfrastructureProvider with
name nico`.

Then create the credentials Secret CAPNICo reads. CAPNICo owns it, and no
chart here writes it:

```bash
kubectl create secret generic nico-credentials -n capnico-system \
  --from-literal=endpoint=<nico-api-base-url> \
  --from-literal=orgID=<org> \
  --from-literal=apiName=<api> \
  --from-literal=token=<token>
```

Leave `--infrastructure nico` off on a cluster that drives no NICo hardware.
Everything else here still installs; nothing reconciles `NicoCluster` or
`NicoMachine`.

See [infrastructure-providers.md](../../docs/site-bootstrap/infrastructure-providers.md)
for the full provider setup, including why `nvkectl get providers` reporting
`CONFIGURED` is a narrower claim than it looks.

`make/tool-versions.mk` also pins `CERT_MANAGER_VERSION`, which records the
cert-manager version `clusterctl init` brings at that CAPI version. It moves
with `CAPI_VERSION`.

Running `clusterctl init` again later is supported and skips what is already
present, so a cluster that has it needs nothing here.

On a cluster that will never run Cluster API, follow
[cert-manager's own installation guide](https://cert-manager.io/docs/installation/helm/)
rather than a command copied from here, so you get the version and flags that
the project supports at the time you read it.

cert-manager is a prerequisite rather than a chart here, and cannot be made
one. It renders its six CRDs as ordinary templates, not in a `crds/`
directory — 12,749 lines of them. Helm validates every manifest in a release
against the API server before applying any of them, so a `Certificate` and the
CRD defining it cannot be in the same release. Vendoring those CRDs into a
`crds/` directory, as [`spire/`](spire/) does for SPIRE's three, would make
this repository the owner of cert-manager's CRD lifecycle. That is not a trade
worth making for a prerequisite `clusterctl init` already installs.

## The sequence

```bash
helm dependency build deploy/charts/spire
helm dependency build deploy/charts/nvke
helm dependency build deploy/charts/nico-prereqs
helm dependency build deploy/charts/nico      # needs `helm repo add nico <repo>`

# 1. SPIRE
helm upgrade --install nvke-spire deploy/charts/spire \
  --namespace spire-server --create-namespace \
  --values deploy/charts/spire/values.yaml --wait

# 2. The NVKE controller, with PostgreSQL and Temporal
helm upgrade --install nvke deploy/charts/nvke \
  --namespace nvke-system --create-namespace \
  --values deploy/charts/nvke/values.yaml \
  --set database.password=<pick one> \
  --set nvke-controller.profiles.image.repository=<repo> \
  --set nvke-controller.profiles.image.tag=<tag> --wait

# 3. NICo's namespace, database, credentials and issuer
ssh-keygen -t ed25519 -f ./ssh_host_ed25519_key -N ""
helm upgrade --install nico-prereqs deploy/charts/nico-prereqs \
  --namespace forge-system --create-namespace \
  --values deploy/charts/nico-prereqs/values.yaml \
  --set database.password=<pick one> \
  --set-file sshHostKey.privateKey=./ssh_host_ed25519_key --wait

# 4. NICo
helm upgrade --install nico deploy/charts/nico \
  --namespace forge-system \
  --values deploy/charts/nico/values.yaml \
  --set nico.global.image.repository=<repo> \
  --set nico.global.image.tag=<tag> --wait
```

Add `--values deploy/charts/<chart>/values-kind.yaml` to each for a kind
cluster.

### Do not trust `--wait` alone

On helm 3.19 `--wait` has returned `STATUS: deployed` within seconds of a
Deployment entering `ImagePullBackOff`. It waited correctly for every other
step measured here, but it has been observed not to. Gate each step on the
workload itself:

```bash
kubectl -n nke-controller rollout status deployment/nke-controller --timeout=10m
kubectl -n forge-system   rollout status statefulset/nico-postgresql --timeout=5m
```

`/readyz` on the controller is the real check — it reports PostgreSQL and
Temporal, so an `ok` means both are actually reachable:

```bash
kubectl -n nke-controller exec deploy/nke-controller -- wget -qO- http://localhost:8080/readyz
# {"status":"ok","checks":{"database":"ok","temporal":"ok"}}
```

## Why there is no umbrella chart

There was one. It could not work, and the reasons are worth keeping:

- **Helm applies hooks before manifests.** NICo's chart hooks need a
  namespace, a database and a Secret. All three would be in the same release,
  created after the hooks that read them. No hook weight fixes this: a
  `Namespace` as a `pre-install,pre-upgrade` hook is **deleted and recreated
  on every upgrade** under helm's default `before-hook-creation` policy —
  measured, with everything in it destroyed — and a `pre-install`-only hook
  does not run on the upgrade that first enables NICo.
- **Helm validates the whole release against the API server before applying
  any of it.** A CRD and a custom resource of that CRD cannot share a release.
- **Temporal's chart has no `namespaceOverride`.** Two Temporals in one
  release land in one namespace and take each other's Service names.

Separate releases make all three disappear. Whatever drives the sequence —
`nvkectl`, a shell script, helmfile, Flux — the charts do not care.

## NICo's chart repository

NICo is published separately from NVKE, so [`nico/`](nico/) names no registry.
Register the repository once, under the name `nico`:

```bash
helm repo add nico <your NICo chart repository>
helm dependency build deploy/charts/nico
```

NICo's images are not public either. Supply
`nico.global.image.repository` and `.tag` that the cluster can pull.

Where there is no Vault, [`nico-prereqs/`](nico-prereqs/) can sign NICo's
service certificates from a cert-manager CA under the name NICo expects of its
Vault PKI issuer. NICo cannot tell the difference. **That is not a substitute
for Vault PKI on a real site** — no revocation, no audit trail, no rotation,
and the CA private key is a Secret in the cluster.

## What Helm does and does not sequence

One Helm release is one apply. Helm sorts by kind and waits for workloads, and
that is all the ordering there is. Four places needed work:

**CRDs and the objects that use them.** Helm validates every manifest in a
release against the API server *before* it applies any of them, so a
`ClusterSPIFFEID` and the CRD that defines it cannot be in one release. The
SPIRE CRDs are therefore vendored into [`spire/crds/`](spire/crds/) rather than
taken as a chart dependency — a `crds/` directory is applied first, with a
discovery refresh after it. Helm never *upgrades* a `crds/` directory, so
moving to a newer SPIRE chart means refreshing them with
[`spire/hack/sync-crds.sh`](spire/hack/sync-crds.sh) and applying the result by
hand.

**The SPIRE controller manager's webhook.** The chart applies its
`ClusterSPIFFEID` objects in the same release that creates the controller
manager serving the webhook that validates them, so on a fresh install the API
server calls a webhook that is not listening yet. `failurePolicy` is set to
`Ignore` outright. The webhook still validates once it is up; it no longer
blocks admission while it is starting.

**Temporal's namespace.** Temporal's chart has no `namespaceOverride` — its
templates follow `.Release.Namespace` — so it lands wherever its chart is
released, and two in one release would take each other's Service names. The
NVKE controller's is in [`nvke/`](nvke/) and lands in `nvke-system`; NICo's is
in [`nico-prereqs/`](nico-prereqs/), off by default, and lands in
`forge-system`. Separate releases, separate namespaces, no collision to
refuse.

**One password, three readers.** PostgreSQL's admin user, both of Temporal's
datastores and the controller's connection string must agree, or the Temporal
schema job crash-loops with `password authentication failed`. The `nvke` chart
owns that Secret and writes it into each of the three namespaces that read it.

Re-running any of these charts is safe: `helm upgrade --install` is idempotent,
and nothing here generates a credential or a key that would change on a second
run.

## Known limits

- **Namespaces are values, not derived.** Several hostnames are written out in
  full in more than one place, because a subchart cannot read a sibling's
  values. Each one carries a `CHANGE WITH …` comment naming the others.
- **NICo's subcharts each carry their own `namespaceOverride`.** There is no
  global that sets them all. `nico/templates/validate.yaml` refuses an install
  where one of them disagrees with `namespaces.nico`, so it cannot drift
  silently.
- **Hostnames are restated across charts.** `nico-prereqs`'s `database.host`
  and `nico`'s `nico.nico-api.databaseConfig.DB_HOST` name the same server, and
  nothing checks that they agree. Same for the credentials Secret name, which
  `nico/templates/validate.yaml` does check — it refuses the install when the
  Secret it names is not in the namespace.
