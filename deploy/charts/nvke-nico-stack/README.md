# nvke-nico-stack

NVKE and NICo and what they run on, in one release.

```bash
helm repo add nico <your NICo chart repository>
helm dependency build deploy/charts/nico            # first
helm dependency build deploy/charts/nvke-nico-stack

ssh-keygen -t ed25519 -f ./ssh_host_ed25519_key -N ""

helm upgrade --install nvke-nico-stack deploy/charts/nvke-nico-stack \
  --namespace nvke-system --create-namespace \
  --set nvke.database.password=<pick one> \
  --set nico-prereqs.database.password=<pick one> \
  --set-file nico-prereqs.sshHostKey.privateKey=./ssh_host_ed25519_key \
  --set nvke-nico.enabled=true \
  --set nvke-nico.nico.global.image.repository=<repo> \
  --set nvke-nico.nico.global.image.tag=<tag>
```

cert-manager and Cluster API are prerequisites. `clusterctl init
--infrastructure nico` brings both, and CAPNICo with them. This release creates
`Certificate` and `ClusterIssuer` objects, so their CRDs must exist first.

## It needs a NICo chart without Helm hooks

Through NICo 2.4 the `nico-api` schema migration Job and its database-config
ConfigMap were `pre-install,pre-upgrade` hooks. Helm applies hooks **before
every ordinary manifest in the same release**, so they ran before the
namespace, the database and the credentials Secret this release creates:

```text
Hook pre-upgrade .../nico-api/templates/configmap.yaml failed:
namespaces "forge-system" not found
```

[infra-controller#8270](https://github.com/dsx-ai-factory/infra-controller/pull/8270)
makes both ordinary manifests and has the Job retry until the datastore
answers. With that, one release works — helm's own apply order puts a
`Namespace` first and a `Secret` long before a `Job`.

Against an older NICo chart, set `nvke-nico.enabled=false` and install
[`../nico-prereqs`](../nico-prereqs/) and [`../nico`](../nico/) as separate
releases. See [the sequence](../README.md#installing-as-separate-releases).

## Namespaces

Release this into `nvke-system`. Temporal's chart has no `namespaceOverride`,
so the NVKE controller's Temporal lands in the release namespace, and
`nvke.nvke-controller.temporal.host` names it.

NICo lands in `forge-system`, which is named twice and must agree:

| Value | Default |
|---|---|
| `nico-prereqs.namespace` | `forge-system` |
| `nvke-nico.nico.<subchart>.namespaceOverride` | `forge-system`, for each of the eleven |

`nico/templates/validate.yaml` refuses an install where any subchart disagrees
with `nico-api`'s, and `nico-prereqs` creates the namespace when it is not the
release namespace. That `Namespace` is only safe to put in this release
because NICo no longer has hooks.

## One Temporal, not two

`nico-prereqs.temporal.enabled` is off. Two Temporals in one release land in
the same namespace and take each other's Service names, and Temporal's chart
offers no way to separate them. `templates/validate.yaml` refuses an install
that enables both. To give NICo a Temporal of its own, install `nico-prereqs`
as a separate release in its own namespace.

## Values

This chart sets only what belongs to the combination. Everything else stays in
the five charts' own `values.yaml`, which this chart does not restate — a copy
here would be a second place to change and a second place to get wrong.

```bash
--set nvke.database.password=...
--set nvke-spire.global.spire.trustDomain=...
--set nvke-nico.nico.global.image.tag=...
```

`nvke-storage` and `nvke-nico` are off by default: kind already has a default
StorageClass, a cluster with real storage brings its own CSI driver, and NICo
needs a chart repository and images that are published separately from NVKE.

## Globals are shared

Helm merges `global:` into every subchart. Three keys are read by more than
one:

| Key | Read by |
|---|---|
| `global.spire.*` | SPIRE's four subcharts |
| `global.image.*` | NICo's subcharts |
| `global.imagePullSecrets` | NICo's subcharts and Bitnami's PostgreSQL |
| `global.nvkeNicoStack` | `nico`, to skip a check that only applies to a separate release |

Everything else is namespaced by owner and does not collide.
