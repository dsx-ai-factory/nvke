# Troubleshooting and FAQ

## Bootstrap

### Bootstrap stalls for 30 minutes then fails with no clear error

The most common cause is incomplete node preparation. `nvkectl bootstrap site` dispatches kubeadm workflows to your nodes via `nvke-agent` gRPC, and if node prep is incomplete it will reach `cluster state status=Provisioning` and sit silently until the timeout. Check:

- containerd is installed and the SystemdCgroup setting is `true` in `/etc/containerd/config.toml`
- Kernel modules loaded: `lsmod | grep -E 'overlay|br_netfilter'`
- CNI plugins present at `/opt/cni/bin/`
- `nvke-agent` is running on all nodes: `systemctl status nvke-agent`
- Port 15005 is reachable from the init node to each join node

See [Pre-Provisioned Nodes](../site-bootstrap/pre-provisioned-nodes.md) for the complete node prep checklist.

### Bootstrap fails with "bootstrap cluster … from an earlier run is still running"

Before deleting the leftover Kind cluster, export the etcd encryption key if it was generated during the failed run — it may be the only recoverable copy. The key is in the controller's namespace of the bootstrap cluster, not the default kubeconfig: `nke-controller`, or `cloud-workflow-engine` if an older `nvkectl` created the cluster. Search all namespaces and save each key from the namespace it is in:

```bash
umask 077
export KIND_EXPERIMENTAL_PROVIDER=nerdctl
kind get kubeconfig --name nvkectl-bootstrap > bootstrap.kubeconfig
kubectl --kubeconfig bootstrap.kubeconfig get secrets -A | grep etcd-encryption-key-
# Save each Secret listed above, from the namespace it is listed in:
kubectl --kubeconfig bootstrap.kubeconfig -n <namespace> \
  get secret <etcd-encryption-key-name> -o yaml > etcd-encryption-key-backup.yaml
```

Store `etcd-encryption-key-backup.yaml` somewhere safe, then delete the cluster. Use `nerdctl` explicitly — a bare `kind delete cluster` looks for the cluster using the Docker provider and exits 0 without deleting anything:

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap
```

### `clusterctl init` fails with a 404 on a GitHub release URL

This happens when `nvkectl` was built with `go build` instead of `make build`. The `capiVersion` linker flag is only set by the Makefile; a plain `go build` leaves it as `dev`, which constructs an invalid GitHub release URL at runtime.

Always build with:
```bash
make -C cli build
```

Verify the version was stamped:
```bash
nvkectl version
```

The output should show a real version string, not `dev`.

### Bootstrap fails with "port 6443 is accepting connections on …"

A previous bootstrap attempt left a control plane on one of your nodes. Reset it before retrying:

```bash
ls /etc/kubernetes/manifests   # kube-apiserver.yaml here means a control plane
sudo kubeadm reset -f
```

### Node never becomes Ready after joining

Check `nvke-agent` on the join node:

```bash
sudo systemctl status nvke-agent
sudo journalctl -u nvke-agent -n 50
```

Confirm the init node can reach the join node on port 15005:

```bash
curl -v telnet://<join-node-ip>:15005
```

For pre-provisioned nodes running without mTLS (development environments only — see note below), confirm `--disable-mtls` is set in `/etc/default/nvke-agent`.

> **Note:** `--disable-mtls` disables transport authentication and encryption on the gRPC connection between the controller and `nvke-agent`. Use it only in isolated development environments. Production deployments use SPIFFE/SPIRE-backed mTLS and do not set this flag.

## Tenant Clusters

### `nvkectl get providers` shows `nico: NOT-CONFIGURED`

CAPNICo is not ready. Check:

1. The `nico-credentials` Secret exists in `capnico-system` on the management cluster
2. The NICo API endpoint in the secret is reachable from the management cluster
3. CAPNICo controller is running: `kubectl get pods -n capnico-system`
4. CAPNICo logs for authentication errors: `kubectl logs -n capnico-system deploy/controller-manager`

### `nvkectl get providers` shows `nico: CONFIGURED` but `CREDENTIALS` is not `HEALTHY`

`CREDENTIALS` is CAPNICo's own last check of the credentials Secret, read from the `NicoIdentity` that names it.

- `FAILING (reason)`: fix what the reason names. `CredentialsNotFound` and `InvalidConfiguration` point at the `nico-credentials` Secret; `AuthenticationFailed` and `AccessDenied` mean NICo rejected or forbade the credentials. CAPNICo rechecks within seconds of a change to the Secret.
- `UNKNOWN (ValidationFailed)`: CAPNICo could not reach NICo, or the check timed out or failed TLS verification. Check the endpoint in the Secret and the network path from the management cluster.
- `STALE`: CAPNICo has not checked when a check was due. `DETAIL` names the cause when nvkectl can see one: the manager is not running, or it watches another namespace or `--watch-filter`. When `DETAIL` names no cause, check whether the manager predates the `NicoIdentity` CRD and needs a restart.
- `PENDING`: CAPNICo has not checked the `NicoIdentity`, or its latest change, yet. Run the command again in a few seconds; if nothing checks it, it turns `STALE`.
- `NO-IDENTITY`: no `NicoIdentity` names the Secret. Create one; see [Infrastructure providers](../../cli/README.md#infrastructure-providers).
- `UNSUPPORTED`: the installed CAPNICo predates `NicoIdentity`.
- `UNREADABLE`: the site kubeconfig could not list `NicoIdentity` objects, and `DETAIL` names the error. If listing is forbidden, grant `list` on `nicoidentities.infrastructure.cluster.x-k8s.io` in the Secret's namespace; the kubeconfig permissions are in [Authenticating to CWE](../../cli/README.md#authenticating-to-cwe).

### `nvkectl create cluster` returns 500

The cluster spec YAML has a placeholder value that was not replaced. Common ones: `orgID`, `poolID`, the release name, and the config profile name. These must exist on your NICo site and management plane respectively. Start from `cli/examples/nico-cluster.yaml` and replace every UUID and profile name for your environment.

### Cluster stuck in `Provisioning` with no progress

Open the Temporal Web UI (see [Observability](observability.md)) and find the active workflow for the cluster. The current activity shows where it is stuck. Common causes:

- NICo has no available capacity for the requested instance type — check your NICo site's allocations
- A configured subnet is exhausted of IP addresses
- CAPNICo cannot reach the NICo API — check `capnico-system` pod logs

### Node stuck in `Pending` after desired count was increased

Auto-provisioning waits for matching capacity. Check:
- Your NICo site has available hosts matching the node pool's instance type
- The configured subnet has free IP addresses
- CAPNICo is installed and its credentials work (`nvkectl get providers` shows `nico: CONFIGURED`, with `CREDENTIALS` `HEALTHY` where CAPNICo serves `NicoIdentity`)

## Management plane

### `nvkectl get clusters` returns 401

Authentication is enabled on the management plane. `nvkectl` mints a bearer token from the kubeconfig you passed to `nvkectl config set-site`. Ensure the `nke-clients/nke-client` service account exists on the management cluster and that the kubeconfig identity has the required permissions. See [Configuring API Authentication](../../controller/docs/configuring-api-authentication.md).

### Nodes are not schedulable after bootstrap

All nodes are control-plane nodes after `nvkectl bootstrap site`, and the command removes the control-plane `NoSchedule` taint from every node once they have all joined. Nodes stay tainted only when that removal failed and the command said so. Remove the taint by hand before installing workloads:

```bash
kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- \
  --kubeconfig <sitename>.kubeconfig
```

### Database pods are `Pending` — no persistent volumes available

The site cluster has no storage class after bootstrap. Install a storage provisioner before deploying the NVKE component stack. Example using local-path-provisioner v0.0.31:

```bash
# Verify the manifest before applying
curl -sL https://github.com/rancher/local-path-provisioner/releases/download/v0.0.31/local-path-storage.yaml \
  | sha256sum
# Apply once verified
kubectl apply -f https://github.com/rancher/local-path-provisioner/releases/download/v0.0.31/local-path-storage.yaml \
  --kubeconfig <sitename>.kubeconfig
```

## General

### How do I check what version of NVKE is running?

```bash
nvkectl version                          # CLI version
kubectl get deploy -n nke-controller nke-controller \
  -o jsonpath='{.spec.template.spec.containers[0].image}' \
  --kubeconfig <sitename>.kubeconfig    # controller image tag
```

### Where are the controller workflow logs?

Port-forward the Temporal Web UI from the management cluster and look for workflows in the `nke-controller` namespace. See [Observability](observability.md).

### How do I get a tenant cluster's kubeconfig?

```bash
nvkectl get kubeconfig <cluster-name> --output-file tenant.kubeconfig
```
