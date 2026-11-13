# nico

NICo itself — step 4 of the [sequence](../README.md#the-sequence).

**Install [`../nico-prereqs`](../nico-prereqs/) into the same namespace first,
and wait for it.** This chart adds nothing but the NICo umbrella chart and its
values.

```bash
helm repo add nico <your NICo chart repository>
helm dependency build deploy/charts/nico

helm upgrade --install nico deploy/charts/nico \
  --namespace forge-system \
  --values deploy/charts/nico/values.yaml \
  --set nico.global.image.repository=<repo> \
  --set nico.global.image.tag=<tag> \
  --wait
```

## Why the prerequisites are a separate release

NICo's chart runs two objects as `pre-install,pre-upgrade` hooks:

| Object | Weight | Source |
|---|---|---|
| The database-config ConfigMap | `-10` | [`nico-api/templates/configmap.yaml#L89`](https://github.com/dsx-ai-factory/infra-controller/blob/main/helm/charts/nico-api/templates/configmap.yaml#L89) |
| The schema migration Job | `-5` | [`nico-api/templates/migration-job.yaml#L13`](https://github.com/dsx-ai-factory/infra-controller/blob/main/helm/charts/nico-api/templates/migration-job.yaml#L13) |

Helm applies hooks **before every regular manifest in their own release**. So
three things this chart cannot provide have to come from an earlier one:

- **the namespace** — the hooks are namespaced
- **the database** — the migration Job connects to it
- **the credentials Secret** — the Job reads `DATASTORE_USER` and
  `DATASTORE_PASSWORD` from it

All three are in [`../nico-prereqs`](../nico-prereqs/).
`templates/validate.yaml` refuses the install when the Secret is missing,
rather than leaving a hook that cannot start.

There is no hook-weight trick that fixes this inside one release. A
`Namespace` annotated as a `pre-install,pre-upgrade` hook is deleted and
recreated on every upgrade under helm's default `before-hook-creation`
policy — measured, taking everything in the namespace with it.

## Namespaces

Each NICo subchart resolves its namespace from its own `namespaceOverride`,
and there is no global that sets them all — so `values.yaml` pins every one of
them. `templates/validate.yaml` refuses an install where any subchart
disagrees with the release namespace.

`nico-machine-a-tron` is the exception: it reads `global.namespaceOverride`,
and a global would reach every other chart in a release, so it is not set.

## What a seed does not get

DPF and RMS are not part of the NICo chart and are not installed. The optional
subcharts — `nico-dsx-exchange-consumer`, `nico-ntp`, `unbound`,
`nico-machine-a-tron`, `nico-postgres-exporter` — are off: a seed runs a
reduced NICo.

## The resource pools are placeholders

`nico-api` **exits at startup** without resource pools, so
`nico.nico-api.siteConfig` is not optional. The four pools in `values.yaml`
are the required minimum and their ranges are examples. Replace them with the
site's address plan.
