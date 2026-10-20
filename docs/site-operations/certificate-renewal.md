# Renew site-management cluster certificates

This runbook is for operators maintaining an NVKE fleet. Use it during planned
site maintenance to renew the site-management cluster's kubeadm-managed **leaf
certificates** under its existing certificate authorities (CAs).

The site-management cluster hosts the NVKE controller, Temporal, databases, and
other site services. Its API and control-plane restarts can interrupt fleet
management operations at that site. Schedule the work with the site's owners
and verify those services before returning the site to normal operation.

Complete one site before moving to the next. Within a site, renew **one
control-plane node at a time**, checking recovery before proceeding. Use your
fleet's existing remote-execution tooling and privileged access; the Bash
commands below describe the work to run on the operator machine and selected
node. Automation must preserve the same ordering and stop on a failed check.

This procedure assumes kubeadm static pods, stacked etcd, the default
`/etc/kubernetes` paths, and local kubeadm CA keys. It is for planned renewal
while the cluster and its current certificates are healthy. Externally managed
CAs, expired certificates, external etcd, and different layouts need their own
procedure. It does not rotate CAs, service-account keys, etcd encryption keys,
kubelet certificates, or application TLS.

[Kubernetes' kubeadm guide](https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-certs/)
requires renewal on every control-plane node and a restart of the control-plane
static pods afterward. Use the kubeadm installed for the cluster's Kubernetes
version; this procedure does not depend on a particular patch release.

## Where to run commands

| Location | Tools and purpose |
|---|---|
| Operator machine | `kubectl` with the site kubeconfig, remote access, and off-node backup storage |
| Selected control-plane node | Installed `kubeadm`, `crictl`, `kubectl`, and `/etc/kubernetes`; use privileged access for node checks and renewal |

Use a Bash shell for the command blocks. If your terminal uses another shell,
run `bash` first. Node commands below run inside a remote session on the
selected control-plane node. Keep a separate operator terminal for API checks.

## 1. Select a site and prepare maintenance

Select the site from your fleet inventory. Have its current administrative
kubeconfig, control-plane node inventory, approved privileged access, and
backup and recovery instructions available. NVKE bootstrap produces a
`<clusterName>.kubeconfig`; use the current credential held by your site's
secret-management process.

Arrange a maintenance window and one owner. Ensure console or SSH access to
every control-plane node remains available if the site's Kubernetes API fails.
Prevent concurrent provisioning, upgrades, node replacements, and other
certificate work through your site's maintenance process. Agree on the maximum
API outage, node recovery deadline, and who will recover a failed node.

Three control-plane nodes do not by themselves provide a highly available
client endpoint: determine whether clients use an init-node IP or a working
VIP/load balancer. For replicated stacked etcd, confirm healthy quorum will
remain while one member stops. A single-control-plane site needs a planned API
and etcd outage.

On the **operator machine**, use Bash and select the site kubeconfig and context
explicitly:

```bash
export KUBECONFIG=/secure/site.kubeconfig
SITE_CONTEXT=site-context
kubectl --context "$SITE_CONTEXT" get namespace kube-system -o jsonpath='{.metadata.uid}{"\n"}'
kubectl --context "$SITE_CONTEXT" get nodes -o wide
kubectl --context "$SITE_CONTEXT" -n kube-system get pods -o wide
kubectl --context "$SITE_CONTEXT" get deployments,statefulsets -A
```

Record the `kube-system` UID, node names/IPs, API endpoint, and workload
readiness against the selected site's inventory. Verify the NVKE controller
API, Temporal workers, database, infrastructure provider, and site identity
services using your deployment's health checks. See
[Observe NVKE and its clusters](../operate/observe.md) for NVKE service diagnostics.

**Operator machine — connect to a control-plane node.** Use your fleet's remote
access method. With ordinary SSH, for example:

```bash
NODE_SSH=operator@control-plane-host  # Replace with your SSH user and host.
ssh "$NODE_SSH"
```

After connecting, run `bash` on the node if its login shell is not Bash.

**Control-plane node — preflight checks.** In that remote Bash session, check
that its `/etc/kubernetes/admin.conf` reaches the recorded UID. Check that the
installed kubeadm uses the cluster's Kubernetes minor version and that all
certificates, the required CA keys, and the etcd health-check client
certificate and key are present:

```bash
sudo kubeadm version -o short
sudo kubectl --kubeconfig=/etc/kubernetes/admin.conf get namespace kube-system \
  -o jsonpath='{.metadata.uid}{"\n"}'
sudo kubeadm certs check-expiration
sudo test -f /etc/kubernetes/pki/ca.key
sudo test -f /etc/kubernetes/pki/front-proxy-ca.key
sudo test -f /etc/kubernetes/pki/etcd/ca.key
sudo test -f /etc/kubernetes/pki/etcd/healthcheck-client.crt
sudo test -f /etc/kubernetes/pki/etcd/healthcheck-client.key
```

Every etcd health command below needs the health-check client pair, so a
missing file would otherwise fail the procedure partway through.

Stop if a required certificate, CA key, or client key is absent, a certificate
is expired or externally managed, or any node, static pod, or management
workload is unhealthy. Use the site's etcd tools to
check the actual member list and health of **every** endpoint; a count of Ready
Kubernetes nodes is not an etcd quorum check. For NVKE's default stacked-etcd
container, use the node's containerd CRI socket and select the site
`kube-system` static pod by its Kubernetes labels. A node can also run other etcd
containers for site workloads. **Control-plane node — etcd health.** Inspect
all listed members. The examples use containerd's usual socket; substitute your
node's CRI endpoint if different. If `/etc/crictl.yaml` already configures the
correct endpoint, you can omit `--runtime-endpoint` throughout this runbook.

First, set `NODE` **inside the node's SSH session** to its exact name from
`kubectl get nodes`, then list its control-plane etcd container. Variables set
on the operator machine are not automatically passed into the SSH session.

```bash
NODE=control-plane-node-name  # Kubernetes node name, not the SSH login.
# Match the control-plane pod: other services can also have etcd containers.
sudo crictl --runtime-endpoint unix:///run/containerd/containerd.sock ps \
  --name '^etcd$' \
  --label io.kubernetes.pod.namespace=kube-system \
  --label "io.kubernetes.pod.name=etcd-${NODE}"
```

Expect one `Running` row. If there are zero rows, check `echo "$NODE"` and list
etcd containers without the label filters (`ps -a --name '^etcd$'`, using the
same CRI endpoint). An empty filtered list alone does not establish that etcd
is stopped. Resolve zero or multiple matches before continuing.
Copy the matching running row's `CONTAINER` ID below.
Run these commands in the same node session:

```bash
ETCD_ID=replace-with-container-id
# Run etcdctl inside the container, using the local endpoint and kubeadm's
# etcd CA, health-check client certificate, and its private key for TLS.
sudo crictl --runtime-endpoint unix:///run/containerd/containerd.sock exec "$ETCD_ID" \
  etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
  --key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
  member list -w table

# Discover and check every member endpoint, not just the local member.
sudo crictl --runtime-endpoint unix:///run/containerd/containerd.sock exec "$ETCD_ID" \
  etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
  --key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
  endpoint health --cluster
```

The member list must match the site's control-plane inventory, and every
endpoint must report `healthy`. Otherwise, repair the baseline before renewal.
Repeat this check after restarting etcd, selecting its new container ID.

## 2. Verify recovery backups

Before any renewal, verify a recent **off-node etcd snapshot** and that the
recovery owner has the matching encryption configuration/key, CA keys, and
restore instructions. A YAML export or filesystem copy of `/var/lib/etcd` is
not a substitute. See [etcd recovery](https://etcd.io/docs/v3.7/op-guide/recovery/)
and the etcd encryption key file in [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md).

Also back up the selected node's `/etc/kubernetes` tree **off-node** before
renewing it. From the operator machine, with approved SSH and noninteractive
sudo, for example:

**Operator machine — node file backup:**

```bash
umask 077
NODE=control-plane-node-name  # Kubernetes node name, used to name the backup.
NODE_SSH=operator@control-plane-host  # SSH user and host for that same node.
BACKUP_DIR=/secure/site-certificate-backups
mkdir -p -m 0700 "$BACKUP_DIR"
ssh "$NODE_SSH" 'sudo -n tar -C / -cpf - etc/kubernetes' > "$BACKUP_DIR/$NODE-kubernetes.tar" && \
  tar -tf "$BACKUP_DIR/$NODE-kubernetes.tar" >/dev/null || \
  { rm -f "$BACKUP_DIR/$NODE-kubernetes.tar"; false; }
```

Continue only if the SSH backup and archive check both succeed. If either
fails, the command deletes the archive so no truncated copy of the CA keys
stays on disk; fix the cause and repeat. Keep the
archive on encrypted, access-restricted storage outside the repository; it
contains live credentials and CA keys.
Repeat this file backup for each node immediately before its renewal. The file
archive protects PKI and manifests; the etcd snapshot protects cluster state.

## 3. Renew one control-plane node

Monitor the site's API and applications **before** the change and through
recovery, using your existing monitoring. If you need a basic authenticated
API check, run this loop in a separate operator terminal with the same
kubeconfig and context. Configure that terminal as in Section 1 and confirm
`kubectl --context "$SITE_CONTEXT" get nodes` succeeds before starting the
loop. Keep those working settings; the loop does not select a different site.

**Operator machine — API monitor:**

```bash
API_REQUEST_TIMEOUT=5s  # Keep this shorter than the agreed maximum API outage.
while :; do
  printf '%s ' "$(date -u +%FT%TZ)"
  if kubectl --context "$SITE_CONTEXT" --request-timeout="$API_REQUEST_TIMEOUT" get nodes -o name >/dev/null; then
    echo OK
  else
    echo FAIL
  fi
  sleep 1
done
```

The request timeout bounds API requests. Each command is followed by a
one-second pause. Keep its output in a private log when an outage record is
needed. On failure, kubectl's error remains visible so you can distinguish an
API outage from a credential or configuration problem. If failures exceed the
agreed outage limit, stop further changes and investigate. Stop this loop with
Ctrl-C after completing maintenance.

On **one selected node**, renew in a stable console or SSH session. Keep the
preflight expiration output to compare with the renewed certificates:

**Control-plane node — renew certificates:**

```bash
sudo kubeadm certs renew all && \
  sudo kubeadm certs check-expiration
```

`renew all` renews unconditionally. If it fails, stop and investigate. A
successful command updates files on disk; running components can still serve
their old certificates until restarted.

**Restart all four components after `renew all`.** Not all components reload
certificates automatically. Run the following Bash block on the **selected
control-plane node**, in the same Bash session where Section 1 set `NODE`.
Set the recovery timeout to the agreed limit in seconds (600 is an example).
The API check uses `NODE` as a hostname; it must resolve to this node and
appear in its API certificate. If it does not, use this node's `InternalIP` from
`kubectl --context "$SITE_CONTEXT" get nodes -o wide` on the operator machine
in the `--server` URL below. Keep `NODE` unchanged for pod selection.

The loop restarts `etcd`, `kube-apiserver`, `kube-controller-manager`, and
`kube-scheduler` in order, checking recovery before each next restart. Keep
the operator API monitor running. If its failures exceed the agreed outage
limit, interrupt this loop with Ctrl-C and investigate before continuing.

```bash
(
  # Abort on unhandled errors or unset variables; the subshell keeps SSH open.
  set -eu
  # Reuse NODE from Section 1; ${NODE:?...} fails before changes if it is unset or empty.
  : "${NODE:?Set NODE as in Section 1 in this SSH session.}"
  RECOVERY_TIMEOUT=600        # Agreed recovery limit in seconds; adjust for your site.

  # Small command wrappers: "$@" forwards the arguments supplied to each call.
  # Use the local CRI socket so container operations work even while the API is down.
  cri() { sudo crictl --runtime-endpoint unix:///run/containerd/containerd.sock "$@"; }
  # Use this node's hostname: a load balancer could hide its failed API server.
  api() { sudo kubectl --kubeconfig=/etc/kubernetes/admin.conf \
    --server="https://$NODE:6443" --request-timeout=5s "$@"; }
  # Return only running IDs (-q). Match the exact component and control-plane pod
  # so another service's etcd container cannot be selected accidentally.
  container() { cri ps -q --name "^$1$" --label io.kubernetes.pod.namespace=kube-system \
    --label "io.kubernetes.pod.name=$1-$NODE"; }

  # Verify hostname resolution, TLS, credentials, and readiness before any restart.
  api get --raw=/readyz
  # Restore each dependency before restarting its users; maintain only this node.
  for component in etcd kube-apiserver kube-controller-manager kube-scheduler; do
    # A failed crictl call must not exit silently under set -e; the check below reports it.
    old=$(container "$component") || true
    # Empty means no match; whitespace means multiple IDs. Both require investigation.
    [[ -n "$old" && "$old" != *[[:space:]]* ]] || { echo "Expected one $component container." >&2; exit 1; }
    echo "Restarting $component ($old)"
    deadline=$((SECONDS + RECOVERY_TIMEOUT))
    # Kubelet recreates the stopped container and loads the renewed certificate files.
    # Allow 30 seconds for a graceful shutdown; the default (-t 0) sends SIGKILL at once.
    cri stop -t 30 "$old"
    last_probe="No unique replacement container is running yet."
    while :; do
      # Retry short readiness failures quietly; show the last result if recovery times out.
      (( SECONDS < deadline )) || { printf '%s recovery timed out.\n%s\n' "$component" "$last_probe" >&2; exit 1; }
      new=$(container "$component") || true  # Treat a failed lookup as "not yet running" and retry.
      # A new ID proves replacement. Also require API and pod readiness; merely
      # seeing a Running container or an old Ready mirror pod is insufficient.
      [[ -n "$new" && "$new" != "$old" && "$new" != *[[:space:]]* ]] &&
        last_probe=$(api get --raw=/readyz 2>&1) &&
        last_probe=$(api -n kube-system wait --for=condition=Ready "pod/$component-$NODE" --timeout=1s 2>&1) && break
      echo "Waiting for $component restart..."
      sleep 2  # Allow startup time without continuously querying the runtime and API.
    done
    if [[ "$component" == etcd ]]; then
      # Check every etcd endpoint before touching the API server: local health alone
      # does not establish cluster health. Use kubeadm's CA and health-check client
      # credentials for TLS; the timeouts bound unreachable endpoint checks.
      cri exec "$new" etcdctl --endpoints=https://127.0.0.1:2379 \
        --cacert=/etc/kubernetes/pki/etcd/ca.crt \
        --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
        --key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
        --dial-timeout=5s --command-timeout=5s endpoint health --cluster
    fi
    # Require the nodes to remain Ready before making the next change.
    api wait --for=condition=Ready nodes --all --timeout=5s
    echo "$component recovered"
  done
)
```

`crictl stop` makes kubelet recreate the container with the renewed files; no
`crictl start` or manifest move is needed. Its default timeout of 0 kills the
container immediately, so the loop passes `-t 30` to let etcd and the API server
shut down gracefully. See
[Kubernetes' static-pod restart example](https://kubernetes.io/docs/tasks/configure-pod-container/static-pod/#observe-static-pod-behavior).
Deleting the mirror pod with `kubectl` does not restart the container.

During startup, expect `Waiting for <component> restart...`, followed by a
recovery message for each component. If the recovery deadline is reached, the
block stops and prints the last readiness probe result. On any error or
interruption, inspect the current state before retrying; resume with only the
unfinished components in the `for` list. For example, if etcd has already
restarted after this renewal and its health checks passed, start the list with
`kube-apiserver`. Proceed to Section 4 only after all four have recovered.

## 4. Verify before the next node

Repeat the site health checks from Section 1 and review the API monitor.
The post-renewal `kubeadm certs check-expiration` output must show later expiry
for the renewed leaf certificates; the CAs are not renewed. That command only
reads the files on disk, so also confirm that the API server **serves** the
renewed certificate. In the node session where `NODE` is set (use the node's
`InternalIP` if `NODE` does not resolve), compare the served leaf with the file
kubeadm renewed:

**Control-plane node — served API server certificate:**

```bash
disk=$(sudo openssl x509 -in /etc/kubernetes/pki/apiserver.crt -noout -fingerprint -sha256)
served=$(echo | timeout 10 openssl s_client -connect "$NODE:6443" 2>/dev/null |
  openssl x509 -noout -fingerprint -sha256)
if [[ -n "$served" && "$served" == "$disk" ]]; then
  echo "OK: serving the renewed certificate"
else
  echo "STOP: served certificate is missing or differs from apiserver.crt" >&2
  false
fi
```

A mismatch means the API server still serves its old certificate or is not
reachable. The block then exits nonzero so automation stops; investigate before
moving on. Recheck application
clients and aggregated APIs used by the site. Copies of `admin.conf` do **not**
update automatically; distribute renewed admin credentials through the site's
secret-management process. See
[Kubernetes' admin certificate guidance](https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-certs/#copying-the-administrator-certificate-optional).

Only after the agreed outage limit and all health checks pass should the owner
repeat the backup, renewal, restart, and verification on the **next** node.
On any failure, leave remaining nodes untouched and preserve backups and
diagnostics for the recovery owner. Restoring an etcd snapshot or old PKI files
is a separate recovery decision, not a routine renewal step.

After each check, approval, renewal, and verification, record the T1 evidence
as described in
[Manual T1 evidence](https://github.com/dsx-ai-factory/nvke/blob/main/controller/docs/credential-audit.md#manual-t1-evidence);
it cannot be recreated after the maintenance window.

## 5. Return the site to service

After the final control-plane node, repeat the site-wide API, etcd, workload,
and client checks. Confirm the NVKE controller API, Temporal workers, database,
infrastructure provider, and identity services have returned to their healthy
baseline. Check for failed or stalled fleet operations during the maintenance
window using the site's monitoring and Temporal workflow history.

Distribute any renewed administrative kubeconfig copies through the site's
secret-management process. Record the site, nodes renewed, new expiry dates,
backup references, and any observed interruption in your maintenance record.
Restore the work paused for maintenance once the site passes its health checks.
Then repeat this procedure for the next site due for renewal.
