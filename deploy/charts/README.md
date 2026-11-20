# Helm charts

[`nvke-nico-stack/`](nvke-nico-stack/) installs NVKE and NICo and what they
run on, in **one release**. The five component charts below are its
dependencies, and each also installs on its own.

| Chart | Installs | Namespace | In the umbrella |
|---|---|---|---|
| [`storage/`](storage/) | A default StorageClass, for a cluster with none | `nvke-storage` | yes, off by default |
| [`spire/`](spire/) | SPIRE, the OIDC upstream CA chain and the two NodePorts | `spire-server` | yes |
| [`nvke/`](nvke/) | The NVKE controller, with the PostgreSQL and Temporal it runs on | `nvke-system` | yes |
| [`nico-prereqs/`](nico-prereqs/) | NICo's database, credentials and certificate issuer | `forge-system` | yes |
| [`nico/`](nico/) | NICo itself | `forge-system` | yes, off by default |

**The umbrella needs a NICo chart that has no Helm hooks.** Through NICo 2.4
the `nico-api` schema migration Job and its database-config ConfigMap were
`pre-install,pre-upgrade` hooks, and helm applies hooks **before every
ordinary manifest in the same release** — so they ran before the namespace,
the database and the credentials Secret that this release creates:

```text
Hook pre-upgrade .../nico-api/templates/configmap.yaml failed:
namespaces "forge-system" not found
```

[infra-controller#8270](https://github.com/dsx-ai-factory/infra-controller/pull/8270)
makes both ordinary manifests and has the Job wait by retrying. With that,
one release works: helm's own apply order puts a `Namespace` first and a
`Secret` long before a `Job`, and the Job retries until PostgreSQL accepts
connections.

Against an older NICo chart, set `nvke-nico.enabled=false` and install
[`nico-prereqs/`](nico-prereqs/) and [`nico/`](nico/) as separate releases
instead — [the sequence](#installing-as-separate-releases) still works and is
what `nvkectl` drives.

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

## One release

```bash
helm repo add nico <your NICo chart repository>
helm dependency build deploy/charts/nico            # first: the umbrella packages it as found
helm dependency build deploy/charts/nvke-nico-stack

ssh-keygen -t ed25519 -f ./ssh_host_ed25519_key -N ""

helm upgrade --install nvke-nico-stack deploy/charts/nvke-nico-stack \
  --namespace nvke-system --create-namespace \
  --set nvke.database.password=<pick one> \
  --set nvke.nvke-controller.profiles.image.repository=<repo> \
  --set nvke.nvke-controller.profiles.image.tag=<tag> \
  --set nico-prereqs.database.password=<pick one> \
  --set-file nico-prereqs.sshHostKey.privateKey=./ssh_host_ed25519_key \
  --set nvke-nico.enabled=true \
  --set nvke-nico.nico.global.image.repository=<repo> \
  --set nvke-nico.nico.global.image.tag=<tag>
```

Release it into `nvke-system`. Temporal's chart has no `namespaceOverride`, so
the NVKE controller's Temporal lands in the release namespace, and
`nvke.nvke-controller.temporal.host` names it. NICo goes to `forge-system`
through `nico-prereqs.namespace` and every
`nvke-nico.nico.<subchart>.namespaceOverride`; `nico/templates/validate.yaml`
refuses an install where those disagree.

## Installing as separate releases

Each chart stands alone, and this is what `nvkectl` drives — it is also the
only way to install against a NICo chart that still uses hooks.

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

## What one release still cannot do

Two of the three original blockers are gone — NICo's hooks through
[#8270](https://github.com/dsx-ai-factory/infra-controller/pull/8270), and the
CRD-before-custom-resource problem by vendoring SPIRE's CRDs into
[`spire/crds/`](spire/crds/), which helm applies before anything else.

One remains. **Temporal's chart has no `namespaceOverride`**, so two Temporals
in one release land in the same namespace and take each other's Service names.
The NVKE controller's is on; NICo's, in `nico-prereqs`, is off.
`nvke-nico-stack/templates/validate.yaml` refuses an install that enables both.
Give NICo a Temporal of its own by installing `nico-prereqs` as a separate
release in its own namespace.

Worth knowing either way: a `Namespace` cannot be a `pre-install,pre-upgrade`
hook. Under helm's default `before-hook-creation` policy it is **deleted and
recreated on every upgrade**, taking everything in it — measured on a kind
cluster. That is why the fix for NICo had to be on the NICo side.

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
