# Management cluster telemetry: troubleshooting

Failures seen while bringing up NVKE telemetry pipelines (NKTP) on the management cluster, with their causes
and fixes. For checks that tell whether anything is wrong, see
[verification](verification.md).

## The Operator crash-loops with "cannot set blockOwnerDeletion"

**Symptom:** `nktp-operator` restarts repeatedly, and `helm install` with
`--wait` never finishes. Its log shows that creating its NetworkPolicy was
forbidden:
`cannot set blockOwnerDeletion if an ownerReference refers to a resource you
can't set finalizers on`.

**Cause:**
1. The Operator creates a NetworkPolicy for its own Pod, owned by its
   Deployment, with `blockOwnerDeletion: true`.
2. Every cluster NVKE builds runs the `OwnerReferencesPermissionEnforcement`
   admission plugin (`defaultEnabledAdmissionPlugins` in
   `controller/internal/kubeadmconfig`). It allows that field only if the
   requester can `update` the owner's `deployments/finalizers`.
3. The Operator's own install manifests grant that permission. The
   OpenTelemetry Operator Helm chart, which NKTP bundles, does not.

Clusters without that plugin, Kind included, never show the problem.

**Fix:** the site values add the grant through
`opentelemetry-kube-stack.opentelemetry-operator.extraObjects`. If the
Operator crash-loops anyway, check that `extraObjects` still has both
entries. A values file that sets `extraObjects` replaces the whole list and
drops the grant.

```bash
kubectl auth can-i update deployments.apps/nktp-operator --subresource=finalizers \
  -n nktp --as=system:serviceaccount:nktp:opentelemetry-operator   # yes
```

## The install stops at nvke-base-config: "`{{ .ClusterName }}` has no value"

**Cause:** the install has no site name. With `--sitespec`, the site spec's `clusterName` is empty; without one, the BootstrapConfig passed with `--config` has no `ClusterName`.
The `nktp-cluster` ConfigMap is rendered from it, and the renderer refuses an
empty value rather than labeling every signal with nothing.

**Fix:** set `clusterName: <site-name>` in the site spec, or add `"ClusterName": "<site-name>"` to the BootstrapConfig.

## The nktp item fails building chart dependencies

**Cause:** `deploy/nvke-deploy.sh` runs `helm dependency build` for the chart
before installing it. That needs egress to `open-telemetry.github.io`,
`prometheus-community.github.io` and `kubernetes-sigs.github.io`.

**Fix:**
- **Allow that egress,** or
- **Build the dependencies ahead of time** with `make nktp-chart-deps` in the
  checkout the path installs from, or
- **Install from a release.** `nvke-deploy.sh install --sitespec … --stack-config … --release …` puts the packaged
  nktp chart the release carries under `charts/`, and `helm dependency build`
  is not run for an archive.

## Collectors don't appear, or the Operator's webhook fails

**Cause:** cert-manager isn't serving yet, so the Operator's webhook
certificate or the Target Allocators' mTLS certificates aren't issued.

**Fix:** check `kubectl -n nktp get certificates` and cert-manager's own
Pods. On the management cluster, `clusterctl init` installs cert-manager before
the deploy path runs; if it was skipped, run it.

## Scrape targets are down

`promql 'count by (job) (up == 0)'`, from
[verification](verification.md#2-the-signals-arrive), lists the failing jobs.
The backend only receives what the Collectors push, so it can't say why. The
Collector that scraped a target logs each failure, with the error and the
target's labels:

```bash
kubectl -n nktp logs -l app.kubernetes.io/component=opentelemetry-collector \
  --prefix --max-log-requests 20 --since 10m \
  | grep 'Failed to scrape Prometheus endpoint'
```

| Target | Cause | Fix |
|---|---|---|
| `kube-scheduler`, `kube-controller-manager` | They listen on `127.0.0.1` on NVKE, and on kubeadm clusters generally | The site values turn these monitors off. Make sure an override didn't turn them back on. |
| `kube-proxy` | OVN clusters run no kube-proxy | Also turned off in the site values |
| `kube-etcd` | etcd serves metrics on `127.0.0.1` only | NVKE sets `listen-metrics-urls: http://0.0.0.0:2381`; elsewhere, turn the monitor off |
| A target that just disappeared | Its monitor was deleted, but Prometheus keeps the last sample for up to five minutes | Wait it out |

## No API audit logs

**Cause:**
- **Wrong path:** the receiver reads a path that doesn't exist. NVKE writes the
  audit log to `/var/log/kubernetes_audit/audit.log`, and the site values
  point `filelog/kube-api-audit` there. A cluster that writes it elsewhere
  needs the same override.
- **Logging off:** API audit logging is off, so there is no file at all.

## No journal logs

**Cause:** the node keeps its journal in `/run/log/journal`, which is
in-memory only, rather than `/var/log/journal`.

**Fix:** mount `/run/log/journal` and point the `journald` receiver's
`directory` at it; see
[configuration](configuration.md#a-different-host-layout). Collection starts
with new entries, so an idle node can take a while to show any.

## The autoscalers show no metrics

**Cause:** nothing serves `metrics.k8s.io`, or two providers are fighting over
it.

**Fix:** the site values turn metrics-server on because a management cluster has
no other provider. If something else later installs a provider, turn one of
the two off. The `v1beta1.metrics.k8s.io` APIService has a single owner, and
two providers make it flip between them.

## Metrics are rejected by a real backend

**Cause:** Target Allocator 0.159.0 exports some counters as delta even though
cumulative temporality is requested.

**Fix:** make the backend accept delta metrics, as Prometheus's
`otlp-deltatocumulative` feature does for the development backend; see
[configuration](configuration.md#connecting-a-real-backend).

## SPIRE OIDC discovery rejects requests by IP after a hand upgrade

**Symptom:**
`https://<site-ip>:30443/.well-known/openid-configuration` stops returning
200 after someone upgraded the `spire` release by hand.

**Cause:** the upgrade used a `deploy/nico` overlay with `--reuse-values`. The
overlay's `spiffe-oidc-discovery-provider.config.additionalDomains` list
replaced the per-site one, which the deploy path had passed last with the
site IP in it.

**Fix:**
1. `helm rollback spire <last good revision> -n spire-server`
2. Apply only the change you meant to make, with `--set`.

In general, change a site through the deploy path rather than release by
release; it passes the per-site values last.

## Chart upgrades don't pick up new CRD fields

**Cause:** the chart ships the OpenTelemetry and Prometheus CRDs in `crds/`,
and Helm installs those once and never upgrades them. After a chart upgrade
that bumps the Operator, an existing site keeps the old CRDs. The API server
then silently drops fields they don't know from new custom resources.

**Fix:** after such an upgrade, apply the CRDs from the new chart package
yourself:

```bash
helm template nktp <nktp-x.y.z.tgz> --include-crds \
  | yq 'select(.kind == "CustomResourceDefinition")' \
  | kubectl apply --server-side -f -
```

## Some images still pull from public registries behind a mirror

**Cause:** `global.imageRegistry` moves the Collector and Target Allocator
images to the mirror, but three images don't read it: metrics-server's, the
OpenTelemetry Operator's, and the kubectl image of opentelemetry-kube-stack's
uninstall hook.

**Fix:** on an air-gapped site, point those three at the mirror through their
own values:

```yaml
metrics-server:
  image:
    repository: <mirror>/metrics-server/metrics-server
opentelemetry-kube-stack:
  opentelemetry-operator:
    manager:
      image:
        repository: <mirror>/open-telemetry/opentelemetry-operator/opentelemetry-operator
  cleanupJob:
    image:
      repository: <mirror>/rancher/kubectl
```

## The chart fails with `image tag ... must be a quoted string`

**Cause:** a numeric-looking tag such as `tag: 1.0` was written unquoted, so
YAML reads it as a number.

**Fix:** quote it: `tag: "1.0"`.
