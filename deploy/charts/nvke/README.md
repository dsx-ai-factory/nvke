# nvke

The NVKE management plane: the NVKE controller, with the PostgreSQL and
Temporal it runs on.

```bash
helm dependency build deploy/charts/nvke
helm upgrade --install nvke deploy/charts/nvke \
  --namespace nvke-system --create-namespace \
  --set database.password=<pick one> \
  --set nvke-controller.profiles.image.repository=<repo> \
  --set nvke-controller.profiles.image.tag=<tag>
```

`--namespace nvke-system` is not arbitrary: Temporal's chart has no
`namespaceOverride`, so it lands in the release namespace, and
`nvke-controller.temporal.host` names `nvke-system`. Release this chart
elsewhere and change that host to match. `NOTES.txt` reports the mismatch.

## Subcharts

| Subchart | From | Off with |
|---|---|---|
| `nvke-controller` | This checkout, `controller/deploy/charts/controller` | `nvke-controller.enabled=false` |
| `postgresql` | Bitnami, `oci://registry-1.docker.io/bitnamicharts` | `postgresql.enabled=false` |
| `temporal` | `https://go.temporal.io/helm-charts` | `temporal.enabled=false` |

The controller chart comes from this checkout rather than a registry, so the
chart and the controller it installs cannot drift. Installing a published chart
instead is a one-line change to `repository` in `Chart.yaml`.

## Values with no default

| Value | Why |
|---|---|
| `database.password` | Nothing in this chart carries a credential. Supply `database.existingSecret` instead to use a Secret created out of band |
| `nvke-controller.profiles.image.repository` and `.tag` | Fleet profile content is maintained outside this repository, so no tag here would mean the same thing on another cluster. Without the image `/api/releases` returns 500 and nothing can be provisioned |

## Namespaces

| Namespace | Holds |
|---|---|
| The release namespace | Temporal |
| `nke-controller` | The NVKE controller |
| `nvke-system` | Objects the controller owns |
| `postgres` | PostgreSQL |

`helm --create-namespace` creates the release namespace only, so this chart
creates the rest. Turn that off with `namespaces.create=false` where something
else owns them.

## One password, three readers

PostgreSQL's admin user, both of Temporal's datastores and the controller's
connection string must agree, or the Temporal schema job crash-loops with
`password authentication failed`. This chart owns that Secret and writes it
into each of the three namespaces that read it, under the three key names each
reader looks for.

## What this chart does not install

Cluster API, cert-manager and CAPNICo, all three of which `clusterctl init
--infrastructure nico` brings, along with the `nico-credentials` Secret CAPNICo
reads; SPIRE, which is [`../spire`](../spire/); and NKTP, so the ServiceMonitor,
metrics NetworkPolicy and tracing are off by default — they need the Prometheus
operator CRDs.
