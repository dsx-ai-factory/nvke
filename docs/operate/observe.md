# Observe NVKE and its clusters

This page describes how to check the health of the NVIDIA Kubernetes Engine (NVKE) management plane and the workload clusters it manages, and where to find their logs, metrics and dashboards. You use it after [Run NVKE end to end on one machine](../get-started/local-site.md), and whenever you need to know whether NVKE and its clusters are working.

## Before you begin

- You have completed [Run NVKE end to end on one machine](../get-started/local-site.md), and `nvkectl config set-site` points `nvkectl` at your NVKE API and the management cluster's kubeconfig. If API authentication is on, `nvkectl` points at an OpenID Connect (OIDC) kubeconfig instead, as described in [Secure the NVKE API](api-authentication.md). With that kubeconfig, `nvkectl get providers` and `nvkectl get kubeconfig` fail, because they read the management cluster's Kubernetes API, which does not accept your identity provider's tokens; the steps that use them give a `kubectl` alternative.
- `kubectl` uses the management cluster's admin kubeconfig, for example with `export KUBECONFIG=<CLUSTER_NAME>.kubeconfig`, where `<CLUSTER_NAME>` is the management cluster's name.
- You have `curl` and `jq`.
- If you change the management plane's values below, run the install from a shell with the `NVKECTL_*` variables exported, as described in [Run NVKE end to end on one machine](../get-started/local-site.md).
- You know `<MANAGEMENT_ADDRESS>`, the management cluster's API virtual IP address (VIP), or the node IP of a single-node management cluster. The NVKE API is at `http://<MANAGEMENT_ADDRESS>:30080`. If API authentication is on, add `-H "Authorization: Bearer <TOKEN>"` to each `curl` command that calls `/api/`, where `<TOKEN>` is a JSON Web Token (JWT) from your identity provider, as described in [Secure the NVKE API](api-authentication.md). `/healthz` and `/readyz` need no token.

## Check the management plane

1. Check that the management plane is rolled out.

   ```bash
   kubectl rollout status deployment/nvke-controller -n nvke-controller
   ```

   The output ends with `successfully rolled out`.

2. Check that the NVKE API is ready.

   ```bash
   curl -s http://<MANAGEMENT_ADDRESS>:30080/readyz
   ```

   A ready management plane returns HTTP 200 and `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`. If a check fails, the endpoint returns HTTP 503 with `"status":"unavailable"` and the error in place of `ok` for that check. The result is cached for 5 seconds. `/healthz` checks nothing beyond the API process serving HTTP.

3. List the management cluster's pods that are not running.

   ```bash
   kubectl get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded
   ```

   A healthy management cluster prints `No resources found`. The management plane depends on the pods in the `nvke-controller`, `postgres`, `temporal`, `spire-server`, `spire-system` and `capnico-system` namespaces.

4. Check the infrastructure provider.

   ```bash
   nvkectl get providers
   ```

   The `nico` row, for NVIDIA Infra Controller (NICo), reads `CONFIGURED` and `REPLICAS` reads `1/1` or more. `CONFIGURED` means the provider's resource definitions exist, its controller is available and its credentials Secret exists. It does not test the credentials against NICo. The `CREDENTIALS` column reports CAPNICo's own check of them where CAPNICo serves `NicoIdentity`; with the CAPNICo release the install deploys, it reads `UNSUPPORTED`. `DEGRADED` means the provider's controller is not available, and `UNKNOWN` usually means your kubeconfig cannot read Secrets in the provider's namespace.

   With API authentication on, this command fails. Check the provider with `kubectl` instead:

   ```bash
   kubectl -n capnico-system get deploy,secret
   ```

   The Deployment `controller-manager` shows all replicas ready, and the Secret `nico-credentials` is listed.

### Read the management plane logs

The management plane writes one JSON object per log line to standard output, with the keys `time`, `level` and `msg`.

1. Show recent warnings and errors.

   ```bash
   kubectl logs deployment/nvke-controller -n nvke-controller --since=1h \
     | jq -cR 'fromjson? | select(.level == "WARN" or .level == "ERROR")'
   ```

2. Follow the logs while you reproduce a problem.

   ```bash
   kubectl logs deployment/nvke-controller -n nvke-controller -f
   ```

The default level is `info`. To log at `debug`, save these values as `controller-log.yaml` in the directory above the `nvke/` checkout:

```yaml
env:
  - name: NODE_AGENT_PORT
    value: "15005"
  - name: LOG_LEVEL
    value: debug
```

Then run the install command from [Run NVKE end to end on one machine](../get-started/local-site.md) again, with the same flags and `--values-for controller=../controller-log.yaml` added. Your `env` list replaces the deploy path's list, which sets `NODE_AGENT_PORT`, so keep that entry. `LOG_LEVEL` accepts `debug`, `info`, `warn` and `error`. If you turned on API authentication, also pass `--values-for controller=../auth-values.yaml`, or the install turns authentication off; see [Secure the NVKE API](api-authentication.md). If you passed `--values-for nktp=<FILE>` when you installed, pass it again, or NKTP goes back to the bundled development backend.

Database queries appear in the `query` key with a `?` for each value, and a PostgreSQL error appears by its severity and code without its message, at every log level. The values can include content you supplied, such as cloud-init files in a cluster's `infra` settings, and an error message can repeat a rejected value. To log the values and messages while you debug a query, add this entry to the `env` list in `controller-log.yaml` and run the install command again as above:

```yaml
  - name: DB_QUERY_LOG_VALUES
    value: "true"
```

> **Warning:** The values and messages appear in the logs in clear text. Remove the entry when you finish.

### Read the management plane metrics

The management plane serves Prometheus metrics at `/metrics` on port 9090 of its pod. NVKE telemetry pipeline (NKTP) scrapes them, as described in [Find the management cluster's telemetry](#find-the-management-clusters-telemetry). To read them directly:

1. Forward the metrics port to your machine.

   ```bash
   kubectl port-forward -n nvke-controller deployment/nvke-controller 9090:9090
   ```

2. In a second shell, list the NVKE series.

   ```bash
   curl -s http://127.0.0.1:9090/metrics | grep '^nvke_'
   ```

The series that answer the most common questions:

| Metric | Labels | What it tells you |
|---|---|---|
| `nvke_controller_cluster_info` | `name`, `status`, `kubernetes_version`, `cni_plugin`, `etcd_encrypted` | One series per workload cluster. `status` is `provisioning`, `provisioned`, `deleting`, `control_plane_upgrading`, `cp_nodes_recycling` or `unknown`. |
| `nvke_controller_node_pool_desired_count`, `nvke_controller_node_pool_actual_count`, `nvke_controller_node_pool_pending_count` | `tenant_cluster`, `node_pool_name`, `node_pool_id` | Desired, created and pending nodes in each node pool. |
| `nvke_controller_node_condition` | `tenant_cluster`, `node`, `condition`, `status` | Each node condition the API reports, such as `kubernetesReady`, with value 1. |
| `nvke_controller_node_lifecycle_elapsed_seconds` | `tenant_cluster`, `node`, `node_type`, `state` | How long a node has been in `pending`, `provisioning`, `deleting` or `markedForRecycling`. A value that keeps growing is a stuck node. |
| `nvke_controller_node_provision_duration_seconds`, `nvke_controller_node_provision_total` | `tenant_cluster`, `node_type`, `result` | Node provisioning time and outcomes. `result` is `success` or `failure`. |
| `nvke_controller_node_provisioning_giveups_total` | `cluster_name`, `node_pool_name` | Worker nodes that NVKE deleted because provisioning took longer than its timeout. |
| `nvke_controller_node_teardown_duration_seconds`, `nvke_controller_node_teardown_total` | `tenant_cluster`, `node_type`, `result` | Node deletion time and outcomes. |
| `nvke_auth_requests_total`, `nvke_auth_enabled` | `outcome`, `reason`, `issuer`; `mode` | API authentication decisions and the active mode. See [Secure the NVKE API](api-authentication.md). |

`node_type` is `cp` for control-plane nodes and `worker` for workers. The `tenant_cluster` label holds the workload cluster's name.

A NetworkPolicy admits scrapes of port 9090 only from a fixed set of namespaces, including `monitoring`, and from the namespaces in the `metrics.networkPolicy.additionalNamespaces` value, which the deploy path sets to `nktp`. To scrape from a Prometheus in another namespace, write that value, with `nktp` and the new namespace in the list, to a values file, and run the install again with `--values-for controller=<FILE>` added. If you turned on API authentication, also pass `--values-for controller=../auth-values.yaml`, or the install turns authentication off; see [Secure the NVKE API](api-authentication.md). If you passed `--values-for nktp=<FILE>` when you installed, pass it again, or NKTP goes back to the bundled development backend.

## Check workload clusters and node pools

1. List the workload clusters.

   ```bash
   nvkectl get clusters
   ```

   With API authentication on, `nvkectl` must use an OIDC kubeconfig for this and the next two steps; with the management cluster's admin kubeconfig every NVKE API call fails.

   A healthy cluster's `STATUS` is `Provisioned`. `Provisioning`, `Updating`, `Upgrading`, `Recycling` and `Deleting` mean an operation is running. `ProvisioningFailed` and `UpdateFailed` mean the last operation failed. [Create a workload cluster](../workload-clusters/create.md) describes each status.

2. Read the conditions of a cluster whose status is not `Provisioned`.

   ```bash
   nvkectl get clusters <WORKLOAD_CLUSTER> -o yaml
   ```

   `<WORKLOAD_CLUSTER>` is the workload cluster's name. Each entry under `conditions` has a `status`, a `lastTransitionTime` and a `message`. The `message` of the condition that last changed usually names the cause.

3. List the cluster's node pools.

   ```bash
   nvkectl get nodepools --cluster <WORKLOAD_CLUSTER>
   ```

   Each pool's `STATUS` is `Provisioned` once the cluster is provisioned, even while its nodes join. `DESIRED` is the node count you asked for.

4. List the cluster's nodes and the conditions NVKE tracks for them.

   ```bash
   curl -s "http://<MANAGEMENT_ADDRESS>:30080/api/nodes?clusterName=<WORKLOAD_CLUSTER>" \
     | jq -r '.nodes[] | [.name, .conditions.provisioned.status, .conditions.kubernetesReady.status, .conditions.nodeAgentReady.status, .conditions.markedForRecycling.status, .conditions.markedForRepair.status] | @tsv'
   ```

   A healthy node reads `True`, `True`, `True`, `False`, `False`: provisioned, `Ready` in Kubernetes, `nvke-agent` healthy, and not marked for replacement or repair. A node is marked for replacement or repair when its Kubernetes node carries the condition `nvke.nvidia.com/NeedsRecycle` or `nvke.nvidia.com/NeedsRepair` (a node last reconciled by an older controller may still carry the `nke.nvidia.com/` form, which NVKE also reads), set by auto mode or by a break-fix system you run. [Manage node pools](../workload-clusters/node-pools.md) describes what NVKE does next.

5. Check the Cluster API objects behind the cluster.

   ```bash
   clusterctl describe cluster <WORKLOAD_CLUSTER> -n nvke-system
   ```

   NVKE creates each workload cluster's Cluster API objects in the `nvke-system` namespace of the management cluster, named after the cluster. The output shows the readiness of the control plane and of each machine, with the reason for any that is not ready.

6. Check the nodes from inside the workload cluster.

   ```bash
   nvkectl get kubeconfig <WORKLOAD_CLUSTER> --output-file <WORKLOAD_CLUSTER>.kubeconfig
   ```

   With API authentication on, this command fails; get the file with `kubectl` as described in [Get the admin kubeconfig](../workload-clusters/access.md#get-the-admin-kubeconfig) instead.

   ```bash
   kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig get nodes
   ```

   Every node is `Ready`. The kubeconfig grants cluster-admin; see [Access a workload cluster](../workload-clusters/access.md).

## Find the management cluster's telemetry

The install deploys NKTP in the `nktp` namespace of the management cluster. NKTP collects the cluster's logs, metrics and Kubernetes Events, scrapes the management plane through the ServiceMonitor `nvke-controller-metrics`, receives its traces, and labels everything with `k8s.cluster.name` set to the management cluster's name.

By default NKTP exports to a bundled development backend, with Grafana, Prometheus, Loki and Tempo in one pod that keeps data in memory. To look at it, forward Grafana to your machine and open `http://127.0.0.1:3000`. This Grafana gives every anonymous user the Admin role, so keep the port forward bound to `127.0.0.1` and use it only for evaluation.

```bash
kubectl -n nktp port-forward svc/lgtm 3000:3000 --address 127.0.0.1
```

To keep the data, send it to your own backend: write a values file as described under "Set a backend" in [Getting started with NKTP](../nktp/getting-started.md), and run the install from [Run NVKE end to end on one machine](../get-started/local-site.md) again with `--values-for nktp=<FILE>` added. If you turned on API authentication, also pass `--values-for controller=../auth-values.yaml`, or the install turns authentication off; see [Secure the NVKE API](api-authentication.md).

NVKE installs neither NKTP nor cert-manager in workload clusters. NKTP collects only from the management cluster, where the install deploys it; for workload clusters you have the management plane's metrics, which describe each cluster, node pool and node as NVKE tracks them. [Getting started with NKTP](../nktp/getting-started.md) describes NKTP itself.

## Import the reference dashboards

The repository includes three Grafana dashboards for NVKE in `controller/deploy/dashboards/`. NVKE does not install them. Import them into the Grafana that reads your backend, or into the bundled development Grafana.

| File | Title in Grafana | What it shows | Data it needs |
|---|---|---|---|
| `nvke-site-controller-health-reference.json` | `Reference - NVKE Management Cluster Health` | Management cluster workloads, SPIRE, cert-manager and node readiness. | kube-state-metrics, from NKTP. |
| `nvke-provisioning-duration-reference.json` | `Reference - NVKE Provisioning Duration` | Node provisioning and teardown time, success against give-up rate, and nodes in progress. | Management plane metrics. |
| `nvke-cluster-upgrade-overview-reference.json` | `Reference - NVKE Cluster Upgrade Overview` | Kubelet versions per cluster, and control-plane and OS profile panels. | kube-state-metrics; management plane metrics for the optional panels. It shows only clusters that export telemetry to the same backend. |

1. In Grafana, open **Dashboards**, then **New**, then **Import**.
2. Upload a file from `controller/deploy/dashboards/`.
3. When Grafana prompts for `DS_PROMETHEUS`, select your Prometheus data source.
4. Import. Importing the same file again updates the dashboard rather than creating a copy.

The `$cluster` variable lists the values of the `k8s_cluster_name` label, which NKTP sets from `k8s.cluster.name`. If the list is empty, the exported data has no cluster name. Each dashboard has a **Requirements and scope** panel; an empty optional row means that data is not scraped, not that something failed.

## Verify

1. In Grafana, open **Explore**, select the Prometheus data source, and run this query:

   ```text
   nvke_controller_cluster_info
   ```

   The result has one series for each workload cluster that `nvkectl get clusters` lists.

2. Open `Reference - NVKE Management Cluster Health` and select the management cluster's name in the `$cluster` list. The panels show data.

## Next steps

- [Troubleshoot](troubleshoot.md) a failure that these checks find.
- [Manage node pools](../workload-clusters/node-pools.md) to replace or scale nodes.
- [Getting started with NKTP](../nktp/getting-started.md) to tune NKTP and scrape your own services.
