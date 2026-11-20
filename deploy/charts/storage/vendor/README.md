# vendor

[`local-path-provisioner`](local-path-provisioner/) is Rancher's chart,
vendored verbatim from https://github.com/rancher/local-path-provisioner.git at **v0.0.31**, under Apache-2.0. Its
LICENSE travels with it.

Do not edit it. Refresh it with:

```bash
deploy/charts/storage/hack/sync-chart.sh [version]
```

Rancher publishes no Helm repository, and the chart lives inside the
provisioner's git repository, which a Helm dependency cannot name. The default
version is `LOCAL_PATH_PROVISIONER_VERSION` from `make/tool-versions.mk`, so
this chart and `deploy/nvke-deploy-local.sh` install the same provisioner.
