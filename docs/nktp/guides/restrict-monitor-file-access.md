# Restrict monitor file access

To enable `denyFSAccessThroughSMs: true`, first replace file references in every
monitor selected by the allocator with Secret or ConfigMap references. Both
allocators default to `false`; the preparation is the same for both pools.
See [rationale and compatibility](../designs/monitor-file-access.md) for why
`denyFSAccessThroughSMs` defaults to `false` and how enabling it protects the
Collector's mounted credentials from disclosure through monitor file references.

## Prepare the monitors

First identify the allocator responsible for each monitor. With the default
selectors:

| Monitor metadata | Allocator | Values key |
| --- | --- | --- |
| `nktp.nvidia.io/large-target: "true"` | Large-target | `large_target_metrics` |
| Label absent or not `"true"` | Node | `daemonset` |

The node allocator also excludes ServiceMonitors with `app` set to
`kube-prometheus-stack-kubelet` or `prometheus-operator-kubelet`. If you customized
monitor or namespace selectors, use those selectors to determine responsibility.
See [Topology](../architecture/topology.md#metrics-scraping) for target assignment
within each pool.

Before enabling the restriction:

1. Inventory **all ServiceMonitors in all namespaces**, including those installed
   by other charts, and audit PodMonitors as well. Check `bearerTokenFile` and
   `tlsConfig.caFile`, `certFile`, and `keyFile`, including inherited scrape-class
   settings. Every monitor selected by an allocator must be free of file
   references before enabling its restriction. Audit the whole cluster so a
   later label or selector change does not introduce an incompatible monitor.
2. [Create a credential Secret](#supply-a-bearer-token-through-a-secret) containing
   a token accepted by the target, then replace `bearerTokenFile` with
   `authorization.credentials`. For TLS file references,
   [reference the serving certificate's CA](#replace-a-ca-file-with-a-configmap-reference)
   through a Secret or ConfigMap, and client certificates and keys through
   Secret references. The [complete vLLM monitors](#vllm-servicemonitor-before-and-after)
   show the bearer-token change. Grant the responsible allocator access to the
   referenced objects. For the node allocator, see
   [Monitor credentials](../components/node-collector.md#monitor-credentials).
   For the large-target allocator, configure its `prometheusCR.secretNamespaces`
   and provide namespace RBAC separately; automatic chart credential grants
   cover only the node allocator.
3. If another chart cannot configure these references, disable its generated
   ServiceMonitors through that chart's values and maintain replacements with
   the same target selection, pool label, ports, paths, and authentication.
   Include the [bundled Kubernetes component monitors](../components/large-target-metrics.md#kubernetes-component-metrics)
   when preparing the large-target pool.
4. Verify that replacements scrape successfully while the setting is still
   `false`. Then [enable the restriction](#enable-the-restriction) for each
   prepared allocator through your normal NKTP Helm upgrade. Check allocator
   logs for [dropped scrape jobs](#recognize-a-denied-scrape-job) and confirm
   that expected targets still report metrics.

## Supply a bearer token through a Secret

For a target that authenticates Kubernetes service-account tokens and delegates
authorization to Kubernetes, such as a
[Kubebuilder metrics server](https://book.kubebuilder.io/reference/metrics.html),
choose a ServiceAccount authorized to read `/metrics`. You can reuse the
Collector's ServiceAccount if it already has that permission, or create a
dedicated metrics-reader ServiceAccount as shown below. Creating a Collector
ServiceAccount does not by itself grant access to an application's metrics.
Then declare a token Secret associated with the chosen ServiceAccount;
Kubernetes populates the Secret with the account's token. Create the
ServiceAccount and its token Secret in the ServiceMonitor's namespace so the
monitor can reference the Secret. This namespace need not match the Collector's;
the example below uses `metrics-monitors`. A `ClusterRole` and
`ClusterRoleBinding` are required because
[`nonResourceURLs` are not namespaced](https://kubernetes.io/docs/reference/access-authn-authz/rbac/).
Targets with other authentication schemes need credentials issued and authorized
by that target instead.

For bearer authentication, the referenced Secret key contains the **actual token**,
without the `Bearer` prefix and its following space. It does not contain a
filesystem path.

NKTP's credential RBAC only grants the **allocator** access to Secrets and CA
ConfigMaps. The chart does not create the example's separate metrics-reader
identity, its target permissions, or its token Secret. Manage those resources
alongside the application's configuration. Creating a ServiceAccount alone does not
create a token Secret on current Kubernetes versions. Declare a Secret with
type `kubernetes.io/service-account-token` and the
`kubernetes.io/service-account.name` annotation; the token controller fills
`data.token` asynchronously. See
[the Kubernetes token-Secret example](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#manually-create-a-long-lived-api-token-for-a-serviceaccount).

Apply the following resources after editing their namespace and names. The
`metrics-monitors` namespace must already exist. You must create the annotated
Secret shown below; the token controller populates it rather than creating it.
Omit its `data` field and do not add it to the ServiceAccount's `secrets` field:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: example-metrics-reader
  namespace: metrics-monitors
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: example-metrics-reader
rules:
  - nonResourceURLs: [/metrics]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: example-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: example-metrics-reader
subjects:
  - kind: ServiceAccount
    name: example-metrics-reader
    namespace: metrics-monitors
---
apiVersion: v1
kind: Secret
metadata:
  name: example-metrics-token
  namespace: metrics-monitors
  annotations:
    kubernetes.io/service-account.name: example-metrics-reader
type: kubernetes.io/service-account-token
```

Kubernetes adds the token, the ServiceAccount UID annotation, and CA/namespace
data to the Secret. The ServiceMonitor references its `token` key directly;
there is no token-copying command or credential value to put in the manifest.

This creates a **long-lived token without a built-in expiry**. Kubernetes does
not periodically rotate it. (Unlike the Collector's projected ServiceAccount
token file, which kubelet rotates automatically, this token Secret is long-lived
and is not automatically rotated.) Deleting the ServiceAccount invalidates its
tokens and triggers cleanup of its token Secrets. Kubernetes recommends short-lived
TokenRequest tokens when automatic renewal is available; that alternative
requires updating the credential Secret before expiry. See
[Managing Service Accounts](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/#manual-secret-management-for-serviceaccounts).

`type: Bearer` tells the Collector to send `Authorization: Bearer <token>`.
The target validates the token and authorizes the ServiceAccount it identifies;
the authentication type itself grants no permissions. Reference the Secret
using `authorization.credentials`, as shown in the complete monitor below.

Include `metrics-monitors` in the responsible allocator's `secretNamespaces`
and grant its Secret access as described above. For TLS authentication, Secret
keys must contain the PEM CA certificate, client certificate, or private key
referenced by `tlsConfig.ca`, `cert`, or `keySecret`, respectively; replace
`caFile`, `certFile`, and `keyFile` with those object references as well.

## Replace a CA file with a ConfigMap reference

The kube-stack API-server ServiceMonitor uses
`caFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt`. Replace that path
with a reference to the **existing `kube-root-ca.crt` ConfigMap in the monitor's
namespace**, key `ca.crt`. Its value is the PEM-encoded CA certificate bundle
used to verify the Kubernetes API server, which Kubernetes also projects into
the service-account volume at that path. See
[Managing Service Accounts](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/).
No token or private key belongs in this ConfigMap.

Before, in the API-server monitor endpoint:

```yaml
tlsConfig:
  caFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  serverName: kubernetes
  insecureSkipVerify: false
```

After:

```yaml
tlsConfig:
  ca:
    configMap:
      name: kube-root-ca.crt
      key: ca.crt
  serverName: kubernetes
  insecureSkipVerify: false
```

Retain your installation's `serverName`, which must match the API server's
serving certificate. The API-server monitor also needs its `bearerTokenFile`
replaced with Secret-backed credentials; changing only `caFile` is insufficient
to enable the deny flag. Grant the allocator ConfigMap `get` access in the
monitor's namespace as well as access to the credential Secret.

For other targets, use the CA that issued **that target's serving certificate**.
A custom CA ConfigMap's referenced key must contain its PEM CA bundle. The
Kubernetes root CA is appropriate for the API server; it does not automatically
validate an application's separately issued or self-signed certificate.

## vLLM ServiceMonitor before and after

These complete examples adapt vLLM's upstream monitor to a Service in
`production-stack-system` and a ServiceMonitor in `metrics-monitors`. Edit the
namespaces and Service label selectors to match your installation. Create the
`example-metrics-token` Secret in `metrics-monitors` using the earlier steps.

Before:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: vllm-operator
  namespace: metrics-monitors
spec:
  namespaceSelector:
    matchNames: [production-stack-system]
  selector:
    matchLabels:
      app.kubernetes.io/name: production-stack
      control-plane: controller-manager
  endpoints:
    - port: https
      scheme: https
      path: /metrics
      interval: 10s
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      tlsConfig:
        insecureSkipVerify: true
```

After:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: vllm-operator
  namespace: metrics-monitors
spec:
  namespaceSelector:
    matchNames: [production-stack-system]
  selector:
    matchLabels:
      app.kubernetes.io/name: production-stack
      control-plane: controller-manager
  endpoints:
    - port: https
      scheme: https
      path: /metrics
      interval: 10s
      authorization:
        type: Bearer
        credentials:
          name: example-metrics-token
          key: token
      tlsConfig:
        insecureSkipVerify: true
```

Both versions retain upstream vLLM's `insecureSkipVerify: true`, which disables
server certificate verification. To verify the vLLM server's certificate,
configure its serving certificate and reference the corresponding CA with
`insecureSkipVerify: false`;
the API-server CA example above applies only when that CA issued the target's
certificate.

## Enable the restriction

After replacing all file references in the selected monitors and verifying that
they scrape successfully, apply this values override through your NKTP Helm
upgrade. Include only the component whose monitors you have prepared if enabling
one pool at a time:

```yaml
daemonset:
  kubernetes:
    targetAllocator:
      prometheusCR:
        denyFSAccessThroughSMs: true
large_target_metrics:
  kubernetes:
    targetAllocator:
      prometheusCR:
        denyFSAccessThroughSMs: true
```

## Recognize a denied scrape job

If `denyFSAccessThroughSMs` is enabled before a monitor's file references are
replaced, the allocator drops its scrape job and logs a warning like this:

```json
{
  "level": "warn",
  "ts": "2026-10-20T21:04:51Z",
  "logger": "setup.prometheus-cr-watcher",
  "caller": "watcher/promOperator.go:687",
  "msg": "dropping scrape config that references arbitrary file path",
  "job": "serviceMonitor/metrics-monitors/vllm-operator/0",
  "reason": "authorization.credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token"
}
```

The `job` identifies the monitor's namespace, name, and endpoint index. The
`reason` shows the generated Prometheus field corresponding to the monitor's
`bearerTokenFile`. This job is dropped before scraping; a failing scrape with
`up=0` is a different outcome.
