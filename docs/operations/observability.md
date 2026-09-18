# Observability

NKE's operational surface spans three layers: the site controller services, the Cluster API infrastructure provider, and the tenant cluster nodes. Observability tooling for each is covered here.

## Site Controller Services

### Temporal Web UI

The Temporal Web UI is the primary window into what NKE is doing. Every cluster provisioning, upgrade, or node replacement runs as a Temporal workflow. When an operation stalls or fails, the Temporal UI shows the current workflow state and the last activity that ran.

Port-forward the Temporal frontend from the site controller cluster:

```bash
kubectl port-forward -n temporal svc/temporal-frontend-headless 8233:8233 \
  --kubeconfig <sitename>.kubeconfig
```

Then open http://localhost:8233 in a browser. Workflows are organized by namespace; NKE workflows run in the `cloud-workflow-engine` namespace.

**What to look for:**
- Workflows stuck in `Running` with no recent activity — usually a stalled infrastructure operation, a node that isn't responding, or a timeout waiting for NICo
- Workflows in `Failed` — expand the history to see which activity failed and what error it returned
- A backlog of `Pending` workflow tasks — indicates a CWE worker health issue

### CWE Logs

The Cloud Workflow Engine (CWE) emits structured JSON logs. Fetch them from the site controller cluster:

```bash
kubectl logs -n cloud-workflow-engine deploy/cloud-workflow-engine \
  --kubeconfig <sitename>.kubeconfig -f
```

Logs include workflow and activity identifiers so a failure in the Temporal UI can be correlated to a CWE log line. Set `LOG_LEVEL=debug` in the CWE deployment environment to increase verbosity.

### CWE REST API Health

The CWE API exposes a health endpoint:

```bash
curl https://<cwe-url>/health
```

A `200 OK` with `{"status":"ok"}` means the API server is up. `nkectl get providers` is the fastest check that both CWE and CAPNICo are healthy — it returns `CONFIGURED` when the infrastructure provider is ready to mint clusters.

### CloudNative-PG (Database)

CWE and Temporal both depend on PostgreSQL managed by CloudNative-PG. Check cluster health:

```bash
kubectl get cluster -n cloud-workflow-engine --kubeconfig <sitename>.kubeconfig
kubectl get cluster -n temporal --kubeconfig <sitename>.kubeconfig
```

Each cluster should show `Cluster in healthy state`. A `Failing` status means a replica is down or a backup is overdue.

## Infrastructure Provider

### CAPNICo Logs

CAPNICo reconciles `NicoCluster` and `NicoMachine` objects and calls the NICo API to allocate and manage bare-metal machines. Logs are in the `capnico-system` namespace:

```bash
kubectl logs -n capnico-system deploy/cluster-api-provider-nico-controller-manager \
  --kubeconfig <sitename>.kubeconfig -f
```

If a tenant cluster is stuck provisioning, CAPNICo logs show whether NICo returned an error, whether machine allocation timed out, or whether a `NicoMachine` is waiting for a node to appear in Kubernetes.

### NICo Site Health

Check the NICo site independently. See the [NICo documentation](https://docs.nvidia.com/infra-controller/documentation/home) for monitoring and health guidance — NKE's provisioning capacity depends entirely on NICo having healthy hosts and available capacity for the configured instance types.

## Tenant Clusters

Tenant clusters are standard Kubernetes clusters. Use `kubectl` with the cluster's admin kubeconfig (obtained with `nkectl get kubeconfig <name>`) to inspect node and workload health.

For node-level health integration with NKE's lifecycle APIs, see [Node Lifecycle](../tenant-clusters/node-lifecycle.md) and [NVSentinel](https://github.com/NVIDIA/NVSentinel).
