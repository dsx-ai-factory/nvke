# Observability

NVKE's operational surface spans three layers: the management plane services, the Cluster API infrastructure provider, and the tenant cluster nodes. Observability tooling for each is covered here.

## Management plane services

### Temporal Web UI

The Temporal Web UI is the primary window into what NVKE is doing. Every cluster provisioning, upgrade, or node replacement runs as a Temporal workflow. When an operation stalls or fails, the Temporal UI shows the current workflow state and the last activity that ran.

Port-forward the Temporal Web UI from the management cluster:

```bash
kubectl port-forward -n temporal svc/temporal-web 8233:8080 \
  --kubeconfig <sitename>.kubeconfig
```

Then open http://localhost:8233 in a browser. Workflows are organized by namespace; NVKE workflows run in the `nvke-controller` namespace.

**What to look for:**
- Workflows stuck in `Running` with no recent activity — usually a stalled infrastructure operation, a node that isn't responding, or a timeout waiting for NICo
- Workflows in `Failed` — expand the history to see which activity failed and what error it returned
- A backlog of `Pending` workflow tasks — indicates a controller worker health issue

### Controller Logs

The NVKE controller emits structured JSON logs. Fetch them from the management cluster:

```bash
kubectl logs -n nvke-controller deploy/nvke-controller \
  --kubeconfig <sitename>.kubeconfig -f
```

Logs include workflow and activity identifiers so a failure in the Temporal UI can be correlated to a controller log line. Set `LOG_LEVEL=debug` in the controller deployment environment to increase verbosity. By default the logs leave out the values of every database query and the message of a PostgreSQL error; [Observe NVKE and its clusters](../operate/observe.md#read-the-management-plane-logs) describes how to include them.

### Controller REST API Health

The NVKE controller API exposes liveness and readiness endpoints:

```bash
curl https://<controller-url>/healthz   # liveness
curl https://<controller-url>/readyz    # readiness
```

A `200 OK` means the API server is up. `nvkectl get providers` shows whether the CAPI infrastructure provider is configured — it returns `CONFIGURED` when the provider's CRDs are present, the controller manager is running, and the credentials Secret exists. Note that `CONFIGURED` does not validate credentials against the provider backend; a separate connectivity issue can still prevent cluster creation even when the status shows `CONFIGURED`.

## Infrastructure Provider

### Infrastructure Provider Logs

The CAPI infrastructure provider reconciles cluster and machine objects and calls the underlying infrastructure API to allocate and manage bare-metal machines. Check the logs of your infrastructure provider's controller manager:

```bash
kubectl logs -n <provider-namespace> deploy/<provider-controller-manager> \
  --kubeconfig <sitename>.kubeconfig -f
```

If a tenant cluster is stuck provisioning, provider logs show whether the infrastructure API returned an error, whether machine allocation timed out, or whether a machine object is waiting for a node to appear in Kubernetes.

### NICo Site Health

Check the NICo site independently. See the [NICo documentation](https://docs.nvidia.com/infra-controller/documentation/home) for monitoring and health guidance — NVKE's provisioning capacity depends entirely on NICo having healthy hosts and available capacity for the configured instance types.

## Tenant Clusters

Tenant clusters are standard Kubernetes clusters. Use `kubectl` with the cluster's admin kubeconfig (obtained with `nvkectl get kubeconfig <name>`) to inspect node and workload health.

For node-level health integration with NVKE's lifecycle APIs, see [Node Lifecycle](../tenant-clusters/node-lifecycle.md) and [NVSentinel](https://github.com/NVIDIA/NVSentinel).
