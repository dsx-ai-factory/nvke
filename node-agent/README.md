# node-agent

The NKE node-agent is a systemd service that provides the ability to issue GRPC
requests to a node.

## Description

The Node-Agent is a simple GRPC server running on a Node that accepts GRPC
requests used to configure a Node to be used as a Kubernetes Cluster Node via
Kubeadm.


Currently supported Kubeadm functionality:

- Kubeadm Init — with **`noCniInstall` omitted or `false`** (the proto default), the agent may run the **legacy in-init CNI** path for **backward compatibility with older SWE (orchestrator) versions**; **new** orchestration should set **`noCniInstall: true`** and use **`InstallCNI`** after init (see below)
- CNI via NKE Debian packages (**`InstallPackages`** installs `nke-cni-ovn-*` / `nke-cni-calico-*` debs); **`InstallCNI`** applies packaged charts/manifests to the cluster after the **charts/manifest deb** is present on the control-plane host (defaults: OVN charts stream `1.4.0`, Calico `3.30.1` when `version` is empty — see **`InstallCNI`** below)
- Kubeadm Join
- Kubeadm Reset

Currently supported additional functionality:

- Get Kubeadm Status
- Support bundle collection (GetSupportBundle)
- Node readiness preflight (`ExecuteNodePreflight`)

## Install/Configure

We use NKE OS Images built on top of BaseOS using Skysmith, however the release
bundles also includes the install scripts used by Skysmith. To install or
upgrade manually simply follow these steps:

1. Transfer the release bundle (tgz bundle) to your destination node
2. Untar/Extract the bundle on the destination node
3. Change directory to the release directory you just extracted
4. As root (or using sudo) run the desired install script:
   a. kubeadm-installer.sh To install kubeadm components
   b. agent-installer to install the nke-agent service

The agent has a number of options flags that can be specified:

```
Usage of nke-agent:
  -audit-log-file string
    	The audit log file name (default "/var/log/nke-agent-audit.log")
  -bundle-server-url string
    	Bundle source URL. Supports http://, https://, file://, or oci:// schemes.
    	Use oci:// prefix for OCI registry (e.g., oci://nvcr.io/j7sbcjl3qgta/development)
    	(default "https://nke-dev.s3.us-west-2.amazonaws.com/")
  -cni-image-registry string
    	OCI registry prefix for CNI component images (optional).
  -disable-mlx5-rescan
    	Disable mlx5 rescan (disables the init routine mlx5 reset proc)
  -disable-mtls
    	Disable mTLS / use plaintext gRPC (development only)
  -index-file-url string
    	Full URL to custom index file, by default will use <bundleServeruRL>/nke_bundles/ubntu_<version>_index.json
  -secure-server-port string
    	The port for the gRPC server (default "15005")
  -spiffe-socket-path string
    	Path to the SPIFFE workload API socket (default "unix:///tmp/spire-agent/public/api.sock"; ignored with -disable-mtls)
  -version
    	Print the version and exit

```
To provide an easy mechanism to specify these flags without modifying the systemd service file, we provide 
an env file that can be used.  By default the env file is written at `/etc/default/nke-agent` and includes an AGENT_ARGS 
variable for setting optional flags.  For example:
`AGENT_ARGS=" --bundle-server-url https://foobar.com/ --disable-mtls"`

To use an OCI registry for bundles instead of HTTP/S3:
`AGENT_ARGS=" --bundle-server-url oci://nvcr.io/j7sbcjl3qgta/development --disable-mtls"`

### Log files

| Path | Description |
|------|-------------|
| `-audit-log-file` (default `/var/log/nke-agent-audit.log`) | Agent audit log (gRPC request logging when using the audit interceptor). |
| `/var/log/nke-execute-scripts.log` | Script execution log: call info and results for `ExecuteScript` and `StreamExecuteScript` (JSON, one line per call). Optional logrotate config: `scripts/nke-execute-scripts.logrotate` (install to `/etc/logrotate.d/`). |
| (via `GetSupportBundle` RPC) | On-demand tgz containing `/var/lib/nke/state.json`, nke-agent journal logs, `/var/log/nke-agent-audit.log`, and `installed-packages.txt` (Debian package inventory). |


## gRPC server reflection

The agent exposes **gRPC server reflection** on the same port as `proto.NodeAgent` (v1 and v1alpha).
With mTLS disabled for local testing, discover RPCs without a local `node_agent.proto`:

```bash
grpcurl -plaintext localhost:15005 list
grpcurl -plaintext localhost:15005 list proto.NodeAgent
```

Production nodes use SPIFFE mTLS; pass the same TLS/SPIFFE options your client already uses for NodeAgent RPCs.

## Available grpc functions

### General Functionality

* `GetKubeadmStatus(KubeadmStatusRequest)`
  Fetches the status of a node (the entire NodeStatus state file,
  poorly named, should be GetNodeStatus)

* `InstallBundle(InstallBundleRequest)`
  Implements the logic to fetch and install the specified 
  package bundle on the node from known/secure source (nke source)

* `GetBundleStatus(GetBundleStatusRequest)`
  Used to fetch the status of a previously issued `InstallBundle` request.
  Takes the RequestID returned by `InstallBundle` and uses it to query the 
  BundleStatus entries for the desired bundle.

* `InstallCollection(InstallCollectionRequest)`
  Fetches a collection from a URL and installs the items listed in the collection.

  We currently support two formats of collection files:
  1. Bundles (.json)
     If the collections file specified is a .json file it's expected to follow the 
     legacy bundles format, and the agent will install the specified bundle tgz 
     files.
  2. Debs (.txt)
     If the collection file specified is a .txt file it's expected to be the 
     simplified new style apt repo format and the agent will just iterate over the 
     list of debs in the text file installing them via `apt install`

* `GetSupportBundle(GetSupportBundleRequest)`
  Gathers node support artifacts into a gzipped tarball and returns it to the caller.
  The bundle includes: `/var/lib/nke/state.json`, the `nke-agent` unit journal log
  (journalctl), `/var/log/nke-agent-audit.log`, `/var/log/nke-execute-scripts.log`,
  and `installed-packages.txt`
  (inventory of installed Debian packages: name, version, status per line).
  You may pass `additional_files` (repeated string) with absolute paths (e.g. `/var/log/myapp.log`)
  files are skipped. The response contains the tgz bytes in `bundle_tgz`.

  Example usage with grpcurl:

* `ExecuteNodePreflight(ExecuteNodePreflightRequest)`
  Runs optional node readiness checks and/or kubeadm init/join preflight. Orchestrators
  (site-workflow-engine) call this after the agent gRPC server is up to confirm
  cloud-init bootstrap finished before cluster operations proceed.

  **Request fields:**
  - **`checkCloudInitStatus`** — runs `cloud-init status`; passes when output contains
    `status: done`. This is the **SWE default**.
  - **`checks`** — optional `fileExists` entries for sentinel files or other paths.
  - **`kubeadmPreflight`** — optional kubeadm init/join phase preflight.

  See [internal/README-NodePreflight.md](internal/README-NodePreflight.md) for design
  details and SWE integration.

### Kubeadm Related Functionality
* `ExecuteKubeadmInit(KubeadmInitRequest)`
  Runs `kubeadm init` on a node. The **`noCniInstall`** field controls in-init CNI
  (JSON `noCniInstall`; default **`false` when omitted**):
  - **Omitted or `false`:** For **backward compatibility**, a successful init may still
    run the legacy **in-init** CNI path (Tigera/Calico/OVN using `cniPluginName`,
    manifests, `tigeraVersion` / `ovnVersion`, `cniConfig`, etc.), unless
    `skipCniPluginInstall` is set. The stand-alone `InstallCNIPlugin` RPC has been
    removed; this path is internal only.
  - **`true`:** CNI is **not** installed during init. Install the network add-on
    afterward with **`InstallCNI`**; this is what **new** SWE / orchestration should use.
  The optional `files` field lets the caller stage prerequisite files on the node
  before `kubeadm init` runs. Each entry must include `path`, `content`, `mode`,
  and `owner`. File content is limited to 1 MiB per file, `mode` must contain
  only permission bits, and `owner` must use `user:group` format.
  **`initConfig`** (full multi-doc YAML) vs **flags**: when `initConfig` is non-empty, kubeadm runs with `--config` only; ensure **ClusterConfiguration.kubernetesVersion** matches the patch you pre-loaded or bundled (SWE render does this via `kubernetesVersion` in `init.yaml.tpl`). When `initConfig` is empty, the agent builds CLI flags and adds **`--kubernetes-version`** from **`kubernetesVersion`** (JSON) if set, else from `kubeadm version -o short` on PATH, unless **`extraArgs`** already pass `--kubernetes-version=...`.

* `InstallCNI(InstallCNIRequest)`
  Deploys packaged CNI Helm charts or static manifests via `helm install` / `kubectl apply`.
  Intended for **one control-plane node** after **`ExecuteKubeadmInit`** completes with **`noCniInstall: true`**.

  **Prerequisite:** Install the charts/manifest Debian package on **this host** beforehand using
  **`InstallPackages`** or `apt` (e.g. **OVN:** `nke-cni-ovn-charts-<stream>` — default stream `1.4.0` when `version` is empty and matches cni-packages; **Calico:** `nke-cni-calico-manifests-<stream>`, default `3.30.1`). **`InstallCNI`** verifies **`dpkg`** registration and returns **`FAILED_PRECONDITION`** if the package is missing.

  Install **image** packages (`nke-cni-ovn-images-<stream>` / `nke-cni-calico-images-<stream>` ) the same way on nodes that require locally pre-loaded images. If the tenant pulls exclusively from their registry at pod schedule time, image debs may be unnecessary. **`InstallCNI`** does **not** inspect containerd (`ctr`/CRI image lists).

  Sending **`cni_apt_role: CNI_APT_ROLE_IMAGES`** is rejected (**`INVALID_ARGUMENT`**) — that path moved to **`InstallPackages`**.

  Deprecated request fields (**`apply_to_cluster`**, **`skip_apt`**) are ignored. **`skip_apt`**
  previously returned **unimplemented** — it does not anymore.

  **OVN**: resolve chart directory (`/etc/dgx/ovn-kubernetes` when `values-nke.yaml` exists, else `Chart.yaml` from `dpkg -L` on the installed charts deb), then `helm install …` using `podNetworkCidr`, `serviceCidr`, `cni_config`, agent **`--cni-image-registry`** via `global.registry` when unset in `cni_config`.

  **Calico**: locate `manifests/tigera-operator.yaml` via `dpkg -L`; optional Tigera YAML overrides; then Helm or `apply-manifests.sh`/`kubectl apply` depending on packaging.

  **`cniVariant`** — **`CNI_VARIANT_OVN`** or **`CNI_VARIANT_CALICO`**; **`UNSPECIFIED`** is treated as **OVN**. **`version`** — strip leading `v`; selects `<stream>` in the **charts/manifest deb** naming; optional `=` pin suffix in **`resolved_apt_package`** echoes the logical spec **for logging/compatibility**.

  Response **`resolvedAptPackage`** is the charts/manifest package logical spec used to locate manifests on disk.

  See [internal/README-CNI-Bundle.md](internal/README-CNI-Bundle.md) for a short
  end-to-end description (KubeadmInit vs CNI and `apply_to_cluster`).

* `ExecuteKubeadmJoin(KubeadmJoinRequest)`
  Implements the logic to execute kubeadm join on a node. The optional `files`
  field stages prerequisite files on the node before `kubeadm join` runs. Each
  entry must include `path`, `content`, `mode`, and `owner`. File content is
  limited to 1 MiB per file, `mode` must contain only permission bits, and
  `owner` must use `user:group` format.

* `ExecuteKubeadmReset(KubeadmResetRequest)`
  Implements the logic to execute kubeadm reset on a node

* `UpdateAndFetchKubeAdmJoinStrings(KubeadmJoinStringRequest)`
  Implements the logic to create an updated join string and returns
  it to the caller to enable use of kubeadm join

* `GetKubeconfig(KubeconfigRequest)`
  Implements the logic to fetch the admin.conf (kubeconfig) from 
  a controlplane node

* `UpdateNKERBAC(UpdateNKERBACRequest)`
  Applies an NKE RBAC manifest to the local
  cluster. Control-plane only. Per-resource outcomes (`OUTCOME_CREATED`,
  `OUTCOME_UPDATED`, `OUTCOME_UNCHANGED`, `OUTCOME_FAILED`) are returned in
  manifest order.
  See the [UpdateNKERBAC](#updatenkerbac) section below.

* `GetKubeadmStatus(KubeadmStatusRequest)`
  Returns node status (role, phase, join info, encryption, etc.) from `state.json`

* `GetKubeadmJoinString(KubeadmJoinStringRequest)`
  Implements logic to attempt pulling the join string 
  that was produced during `kubeadm init`, *NOTE* this 
  is only available on the node that init was performed 
  on; in most cases you'll need to run `UpdateAndFetchKubeAdmJoinStrings`

 * `InitializeKubernetesAPIEncryption(
   InitializeKubernetesAPIEncryptionRequest)`
   Used for setting up Kubernetes API/etcd encryption on a 
   controlplane node

* `RewriteKubernetesAPIObjects(RewriteKubernetesAPIObjectsRequest)`
  Used as part of the process for enabling Kubernetes API/etcd encryption

* `RemovePlainTextSecondaryEncryptionEntry(
  RemovePlainTextSecondaryEncryptionEntryRequest)`
  Used as part of the process for enabling Kubernetes API/etcd encryption
  During initial configuration a plain text secondary key is assigned (no encrpytion)
  in order for cp nodes that have not been upgraded to be able to read etcd still
  this is the final step in the process and removes that secondary key

* `UpdateEncryptionConfig(UpdateEncryptionConfigRequest)`
  Applies a single encryption-config key-rotation mutation and waits for apiserver reload

* `StartEtcdEncryptionAtRestObjectsRewrite` / `GetEtcdEncryptionAtRestObjectsRewriteStatus`
  Asynchronous rewrite of API objects under the current encryption config

* `RestartAPIPods(RestartAPIPodsRequest)`
  Used as part of the process for enabling Kubernetes API/etcd encryption

### Full Disk Encryption
* `ConfigureTPMUnlockForFDE(ConfigureTPMUnlockForFDERequest)`
  Provides a mechanism to perform the final configuration of 
  full disk encryption on a node; used to convert the nodes 
  encryption from the initial bootstrap password encryption to 
  TPM based encrytpion.

### Random Do Anything Calls
* `UploadFileBundle(stream FileChunk)`
  Provides a mechanism to upload any file bundle to a node

* `ExecuteScript(ExecuteScriptRequest)`
  Provides a mechanism to execute any script on a node. Call details and results
  are logged to `/var/log/nke-execute-scripts.log` (JSON, one line per call).

* `StreamExecuteScript(ExecuteScriptRequest)`
  Provides a mechanism to execute any script on a node using a streaming gRPC
  connection. Call details and completion (including exit code) are logged to
  `/var/log/nke-execute-scripts.log`.

* `SetUnsetIsMaster(SetUnsetIsMasterRequest)`
  Provides the ability to set or remove the `isMaster` parameter of a node
  (internal label that's no longer enforced in most cases)


## Creating a cluster

The primary use case for the node-agent is to create a Kubernetes cluster using
Kubeadm. The node-agent is designed to be run on a node that has been
"bootstrapped" (kubeadm has been installed).

Steps:

- Initialize a node as the Control Plane Master
  `grpcurl -plaintext -d '{\
"podNetworkCidr": "192.168.0.0/16", \
"controlPlaneEndpoint": "10.85.140.165"}' \
10.85.140.165:15005 proto.NodeAgent.ExecuteKubeadmInit`
  (Add **`"noCniInstall": true`** to the JSON if you use **new** SWE; older SWE
  often omits it and may rely on the default legacy in-init CNI. See `ExecuteKubeadmInit` above.)

- Install the CNI **after** init (and typically join other nodes) using
  `InstallCNI` when you set **`noCniInstall: true`**. Typical pattern for **new** orchestration:
  1. On **each** node, `cniAptRole: IMAGES`, `applyToCluster: false`, same `cniVariant` / `version` (OVN: `nke-cni-ovn-images-<stream>`; Calico: `nke-cni-calico-images-<stream>`; omit `cniVariant` for default OVN).
  2. On **one** control-plane node, `cniAptRole: CHARTS`, `applyToCluster: true`, same variant / version (installs `nke-cni-ovn-charts-<stream>` or `nke-cni-calico-manifests-<stream>` and deploys to the cluster), with `podNetworkCidr` / `serviceCidr` as needed.

  Example — image import only (repeat on every node):

  ```
  grpcurl -plaintext -d '{
    "cniVariant": "CNI_VARIANT_CALICO",
    "cniAptRole": "CNI_APT_ROLE_IMAGES",
    "version": "3.30.1",
    "applyToCluster": false
  }' 10.85.140.165:15005 proto.NodeAgent.InstallCNI
  ```

  Example — chart deploy on the designated control-plane node:

  ```
  grpcurl -plaintext -d '{
    "cniVariant": "CNI_VARIANT_CALICO",
    "cniAptRole": "CNI_APT_ROLE_CHARTS",
    "version": "3.30.1",
    "applyToCluster": true,
    "podNetworkCidr": "192.168.0.0/16",
    "serviceCidr": "10.96.0.0/12"
  }' 10.85.140.165:15005 proto.NodeAgent.InstallCNI
  ```

- Fetch the Kubeadm Join String

```
grpcurl -plaintext 10.85.140.165:15005 proto.NodeAgent.GetKubeadmJoinString

{
  "controlplaneJoinString": "  kubeadm join 10.85.140.165:6443 --token qvlp5x.ucye19d1h901jy4d --discovery-token-ca-cert-hash sha256:367ebaaaa04edcd0c8d45aab65f32794c9f50304669bba5b727ff957e564f95d --control-plane --certificate-key 5f20c1202762e9c74945f6ec12e18556d9505de5ff914215d72025d9e904a392",
  "workerJoinString": "kubeadm join 10.85.140.165:6443 --token qvlp5x.ucye19d1h901jy4d --discovery-token-ca-cert-hash sha256:367ebaaaa04edcd0c8d45aab65f32794c9f50304669bba5b727ff957e564f95d "
}
```

- Join additional Control Plane nodes
  `grpcurl -plaintext -d \
'{"kubeadmJoinString": "join 10.85.140.165:6443 \
--token qvlp5x.ucye19d1h901jy4d \
--discovery-token-ca-cert-hash sha256:367ebaaaa04edcd0c8d45aab65f32794c9f50304669bba5b727ff957e564f95d \
--control-plane \
--certificate-key 5f20c1202762e9c74945f6ec12e18556d9505de5ff914215d72025d9e904a392"}' \
10.85.140.166:15005 proto.NodeAgent.ExecuteKubeadmJoin`

- Join a node with a prerequisite file staged under `/etc/kubernetes`

```bash
grpcurl -plaintext -d '{
  "kubeadmJoinString": "join 10.85.140.165:6443 --token qvlp5x.ucye19d1h901jy4d --discovery-token-ca-cert-hash sha256:367ebaaaa04edcd0c8d45aab65f32794c9f50304669bba5b727ff957e564f95d",
  "files": [
    {
      "path": "/etc/kubernetes/custom/join-config.yaml",
      "content": "ZW5hYmxlZDogdHJ1ZQo=",
      "mode": 420,
      "owner": "root:root"
    }
  ]
}' 10.85.140.167:15005 proto.NodeAgent.ExecuteKubeadmJoin
```

- Join Worker nodes

`grpcurl -plaintext -d \
  '{"kubeadmJoinString": "join 10.85.140.165:6443 \
  --token qvlp5x.ucye19d1h901jy4d
  --discovery-token-ca-cert-hash sha256:367ebaaaa04edcd0c8d45aab65f32794c9f50304669bba5b727ff957e564f95d"}' \
  10.85.140.167:15005 proto.NodeAgent.ExecuteKubeadmJoin`



## Dev Environment

The NKE Node-Agent is designed to be Infrastructure agnostic, as such it can be
run on Forge Machines or and Linux host.

For development, it's recommended to use either Multipass, or LXC Virtual
Machines that support cloud-init. Using the sample cloud-init snippet below you
automate the creation of a VM with the Node-Agent installed and bootstrapped
ready to accept requests.

### Sample cloud-init

```
config:
  user.user-data: |
    #cloud-config
    package_upgrade: true
    runcmd:
      - [ sh, -c, "mkdir -p /usr/local/nke/bin" ]
      - [ sh, -c, "wget https://nke-dev.s3.us-west-2.amazonaws.com/node-agent/node-agent -O /usr/local/nke/bin/node-agent" ]
      - [ sh, -c, "chmod +x /usr/local/nke/bin/node-agent" ]
      - [ sh, -c, "/usr/local/nke/bin/node-agent > /var/log/node-agent.log 2>&1 &"]
```

If you just want to set up a Linux machine manually you can execute the shell
commands in the above `runcmd` section manualy as well, or copy the snippet
below into your own bash script and use it.

```
#!/usr/bin/bash
pkill node-agent
mkdir -p /usr/local/nke/bin
wget https://nke-dev.s3.us-west-2.amazonaws.com/node-agent/node-agent -O /usr/local/nke/bin/node-agent
chmod +x /usr/local/nke/bin/node-agent
/usr/local/nke/bin/node-agent >> /var/log/node-agent.log 2>&1 &
```

_NOTE_ We are stil in the early development phase and while we try and update
the S3 bucket referenced above for the agent binary it may not be the latest, or
if you've made changes to the proto code it obviously won't include your
changes. You may need to store your own local version of the binary somewhere or
manualy transfer it if needed. Going forward we will enable storage on NVCR.

By default the GRPC server will start without TLS enabled and will listen on
port 15005, you're now ready to issue requests either via the Node-Agent Client,
or simply using grpcurl.

### Sample usage with grpcurl

Execute kubeadm init on a bootstrapped node:

```
grpcurl -plaintext -d '{"podNetworkCidr": "192.168.0.0/16", "controlPlaneEndpoint": "10.192.200.102"}' 10.192.200.102:15005 proto.NodeAgent.ExecuteKubeadmInit
```

Omitting `noCniInstall` matches **older SWE** behavior (a legacy in-init CNI may run
after a successful init). For **new** SWE, pass **`"noCniInstall": true`** and
install CNI with `InstallCNI` as in **Creating a cluster** above.

Execute kubeadm init with a prerequisite file staged under `/etc/kubernetes`:

```bash
grpcurl -plaintext -d '{
  "podNetworkCidr": "192.168.0.0/16",
  "controlPlaneEndpoint": "10.192.200.102",
  "files": [
    {
      "path": "/etc/kubernetes/custom/auth-config.yaml",
      "content": "aXNzdWVyOiBodHRwczovL2lzc3Vlci5leGFtcGxlLmNvbQo=",
      "mode": 420,
      "owner": "root:root"
    }
  ]
}' 10.192.200.102:15005 proto.NodeAgent.ExecuteKubeadmInit
```

With **`"noCniInstall": true`**, the agent does not install a CNI during init; use
`proto.NodeAgent.InstallCNI` on each node (image import) and once on a
control-plane node (full install), as in the **Creating a cluster** section above.
The examples on this page omit `noCniInstall` for brevity; add it when you follow the **new** flow.

Check the status:

```
grpcurl -plaintext 10.192.200.102:15005 proto.NodeAgent.GetNodesKubeadmStatus
{
  "nodeKubeadmStatus": "Ready",
  "nodeKubeadmPhase": "Initialize",
  "nodeKubeadmCurrentMessage": "Node is initialized",
  "nodeKubeadmCompletedPhases": [
    "Initialize"
  ]
}
```

# Build

Clone the repo and run make... this will build the protos, an agent binary and
the nactl cli binary.

# Install

Steps outlined above, for current release binaries can also be downloaded from
S3:

- node-agent: <https://nke-dev.s3.us-west-2.amazonaws.com/node-agent/node-agent>

### Build a controlplane

```
grpcurl -plaintext -d '{"podNetworkCidr": "192.168.1.0/16", "controlPlaneEndpoint": "10.23.244.5"}' 10.23.244.5:15005 proto.NodeAgent.ExecuteKubeadmInit
grpcurl -plaintext -d '{"kubeadmJoinString": "join 10.23.244.5:6443 --token w4f949.v9p03wgj7h54tw5j --discovery-token-ca-cert-hash sha256:1564b36c0639104a8b096579d2e6c0184a631b621ee5772013c95f4a09213f79 --control-plane --certificate-key 0210dda0020e60298689fb5ed464abef6cc74b6d6fc5edba8659d1c79a993137}' 10.23.244.153:15005 proto.NodeAgent.ExecuteKubeadmJoin
```

Repeat ExecuteKubeadmJoin for additional CP nodes, then install the CNI with
`InstallCNI` (image import on each node, full install on one control-plane
node) as described under **Creating a cluster**—if you used **`noCniInstall: true`**
on init. Older SWE that rely on the default in-init CNI do not set that field.

## Using mTLS

By default the agent serves gRPC with **SPIFFE mTLS**: it loads X.509 identities from the SPIRE workload API
(default socket: `-spiffe-socket-path`). Callers must present a client SVID authorized by the server trust-domain
policy in `cmd/agent/main.go`.

For **local development** only, `-disable-mtls` serves plaintext gRPC (never use this in production).

## Encryption

### Kubernetes API Secrets Encryption

The Node Agent supports the management of Kubernetes API secrets encryption
once a clusters controlplane has been created.

The current implementation requires a sha=256 encrypted secretbox compatable key
to be provided.

1. InitializeKubernetesAPIEncryption (MUST BE RUN ON EACH CONTROLPLANE NODE)

   ```
   grpcurl -plaintext -d '{"encryptionKey": "VT5DZkflKh4DPHp4eZTCOYKzaUN5p6kIcXz50OGy1/U=", "objectsToEncrypt": "secret"}' 127.0.0.1:15005 proto.NodeAgent.InitializeKubernetesAPIEncryption
   ```

   The above grpc request will:

   - create an encryption-config file that includes the passed in encrypted
     secretbox key to be used
   - insert a secondary plain text key to preserve access until rewrite has been
     completed
   - modify the kube-apiserver manifest on this node to point to the
     encryption-config file and enable auto restart on config change
   - force restart of the api pod for initial config changes to be picked up

2. RewriteKubernetesAPIObjects (RUN ON A SINGLE CONTROLPLANE NODE)

   ```
   grpcurl -plaintext 127.0.0.1:15005 proto.NodeAgent.RewriteKubernetesAPIObjects
   ```

   The above grpc request will:

   - issue a rewrite of the secrets using previously configured secretbox key

3. RemovePlainTextSecondaryEncryptionEntry (MUST BE RUN ON EACH CONTROLPLANE
   NODE)

   ```
   grpcurl -plaintext 127.0.0.1:15005 proto.NodeAgent.RemovePlainTextSecondaryEncryptionEntry
   ```

   The above grpc request will:

   - remove the secondary plain text key entry from the encryption-config file
     on the node

### Full Disk Encryption and auto unlock via TPM

The Node Agent supports the management of Full Disk Encryption and auto-decrypt
through the use of TPM PCRs.

There are some assumptions made about the state of the node and how FDE was
setup:

1. FDE is enabled on the root Disk
2. Initial FDE aut-decrypt was setup using keyless-entry
   - A known master key is stored in the luks keystore
   - A randomly generated boot key is stored on the boot partition
   - The /etc/crypttab file is configured to use the boot partition keey to auto
     decrypt on boot
3. FDE is enabled using LVM

While we currently assume the node is bootstrapped with keyless-entry; in the
future we won't require this. For now we use this assumption to make setup and
configuration easier. In general our only requirement is that the Instance was
created with the root disk encrypted and we have access to a master password for
unlock the encryption (needed to add unlock keys).

Once the Instance has succesfully booted and the node agent is running,
administrators can issue calls to setup TPM unlock of the encrypted root device:
`ConfigureTPMUnlockForFDE(ConfigureTPMUnlockForFDERequest)`

Where ConfigureTPMUnlockForFDERequest contains:
`message ConfigureTPMUnlockForFDERequest {
  string current_master_key = 1;
  string new_master_key = 2;
  repeated string pcr_ids = 3;
}`

The above request will ONLY run on an Instance if the following conditions are
met:

1. keyless-entry was used for instance creation to initialize the node
   encryption and decrypt
2. keyless-entry hasn't already been disabled and unconfigured
3. TPM unlock has not already been configured

The proces of TPM Unlock enable is fairly simple and consists of the following
steps:

1. Verify keyless-entry is configured and enabled
2. Evaluate the system to find the root disk and ensure it's marked as encrypted
3. Configure TPM Unlock using clevis bind to PCRs ["1", "4", "5", "7"] ()\*
   currently we only use these 4 regardless of input)
4. Remove the initial keyless-entry configuration we used to bootstrap the
   Instance
5. Add our new master key to the luks key store
6. Remove our "well known" bootstrap luks key

## UpdateNKERBAC

`UpdateNKERBAC` applies a YAML (or JSON) manifest of NKE-owned RBAC objects to
the local cluster. It is intended only for managing NKE RBAC objects: only a
fixed allowlist of resource kinds and `metadata.name` values is accepted.

The RPC must be served by a control-plane node.

### Allowed resources

The agent accepts only these (kind, name) pairs:

| Kind                 | `metadata.name` |
| -------------------- | --------------- |
| `ClusterRole`        | `nke:cwe`       |
| `ClusterRoleBinding` | `nke:cwe`       |

Additional manifest rules:

- The manifest may be a single document or a multi-document YAML stream
  separated by `---`. Empty documents are skipped, but at least one document
  must remain after they are removed.
- A single request must not contain duplicates of the same allowed resource.

The agent does not semantically validate `ClusterRole.rules`,
`ClusterRoleBinding.roleRef`, `ClusterRoleBinding.subjects`, labels, or
annotations; those fields are caller-owned.

### Validation behavior

Validation is all-or-nothing. The entire manifest is validated before any
resource is applied. If any document is malformed or falls outside the
allowlist, the RPC returns `InvalidArgument` and applies nothing.

### Apply behavior

Once validation passes, resources are applied sequentially in manifest order.
Each apply is reported in `UpdateNKERBACResponse.results` with one of:

| Outcome               | Meaning                                                                  |
| --------------------- | ------------------------------------------------------------------------ |
| `OUTCOME_CREATED`     | The resource did not exist and was created.                              |
| `OUTCOME_UPDATED`     | The resource existed and the apply changed it.                           |
| `OUTCOME_UNCHANGED`   | The resource existed and the apply was a no-op.                          |
| `OUTCOME_FAILED`      | The apply failed; `error_message` carries the underlying error text.     |

If a resource fails to apply, the RPC stops, returns the partial results with
the `OUTCOME_FAILED` entry last, and does not roll back earlier successes.

### Error codes

| Code                 | Reason                                                                                             |
| -------------------- | ---------------------------------------------------------------------------------------------------|
| `FailedPrecondition` | The node is not eligible to serve this RPC (e.g. not a control-plane node, kubeconfig not present).|
| `InvalidArgument`    | The manifest is empty, malformed, or contains a resource outside the NKE RBAC allowlist.           |
| `Internal`           | The agent could not reach the local cluster to perform the apply.                                  |

### Example

Build a multi-document manifest and send it as the (base64-encoded) `manifest`
field. With `grpcurl`, `bytes` fields take base64-encoded strings in JSON input:

```bash
MANIFEST=$(cat <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: nke:cwe
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nke:cwe
subjects:
  - kind: User
    name: cwe
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: nke:cwe
  apiGroup: rbac.authorization.k8s.io
EOF
)

jq -n --arg manifest "$MANIFEST" '{manifest: ($manifest | @base64)}' \
  | grpcurl -plaintext -d @- 127.0.0.1:15005 proto.NodeAgent.UpdateNKERBAC
```

Sample response:

```json
{
  "results": [
    { "kind": "ClusterRole",        "name": "nke:cwe", "outcome": "OUTCOME_CREATED" },
    { "kind": "ClusterRoleBinding", "name": "nke:cwe", "outcome": "OUTCOME_CREATED" }
  ]
}
```

Re-running the same request once the resources already exist will return
`OUTCOME_UNCHANGED` for both entries.

## File Upload

The Node Agent includes a grpc call to upload and extract archive files to the
node. One example use case of this call is to upload a script bundle and install
it on the node so that scripts can then be called from the "executeScript" grpc
call.

The upload grpc call is a streaming call, and uses the FileChunk message to
specify the filename and where the file bundle should be extracted to on the
node:

```
{
  Destination: "/usr/local/my-scripts",
  Filename: "my-scripts.tar.gz",
}
```

The agent will:

- create a file in `/tmp/nke_uploads/`
- extract the file to the specified Destination
- delete the uploaded file from `/tmp/nke_uploads/`

See sample-upload-client/main.go for an example to get started.

Files MUST be in tar.gz format (we enforce \*.tgz naming) and are uploaded:
/tmp/nke_uploads

# Node Profiles

Provides the ability to apply a node-profile via a json file in the known NKE
file servers. These profiles can be used to specify various combinations of
software bundles to install on a node and are currently based on the 'type'
of the node (controlplane, worker etc).

Example default profile:

```
{
  "name": "default-v0.0.1",
  "version": "0.0.1",
  "summary": "default-v0.0.1",
  "description": "default-v0.0.1",
  "bundles": [
    {
      "name": "kubeadm",
      "version": "v1.31.9",
      "installCmd": [
        "python",
        "kubeadm-install.py"
      ],
      "installFile": "kubeadm-install.py",
      "useStaging": true
    },
    {
      "name": "display-mode-selector",
      "version": "v1.67.0",
      "installCmd": [
        "/bin/bash",
        "-c",
        "cp displaymodeselector /usr/bin/ && chmod 755 /usr/bin/displaymodeselector"
      ],
      "installFile": "",
      "useStaging": true
    },
    {
      "name": "etcd-manager",
      "version": "v1.0.3",
      "installCmd": [
        "./install.sh"
      ],
      "installFile": "install.sh",
      "useStaging": true
    }
  ]
}

```

Note that the bundles are bundles that exist in the nke_bundles repo and the
profile includes the `installCmd` which is the string array needed to be passed
in to the agent cmd runner.

Example call using grpcurl:

```
grpcurl -plaintext -d '{"collectionURL": "https://example/collection.json"}' 10.179.248.55:15005 proto.NodeAgent.InstallCollection
```

Adds a ProfileStatus to the node-state:

```
  "profileStatus": {
    "status": "success",
    "processedAt": "2025-07-08T19:59:35Z",
    "name": "",
    "type": "default",
    "variant": "",
    "version": "v0.0.1",
    "failedItems": [
    ],
    "appliedItems": [
      "kubeadm:v1.31.9@2025-07-08T19:53:38Z"
      "display-mode-selector:v1.67.0@2025-07-08T19:55:18Z",
      "etcd-manager:v1.0.3@2025-07-08T19:55:19Z"
    ]
  }

```

```
grpcurl -plaintext 10.179.248.107:15005 proto.NodeAgent.GetKubeadmStatus
# or: GetSupportBundle for a fuller diagnostic archive

```

# InstallCollection

`InstallCollection` installs multiple bundles or deb packages in one request by fetching a collection (either .json or .txt) from a URL. This is useful for bootstrapping nodes with a predefined set of components (e.g., kubeadm, CNI, etcd-manager) calls.

There are two formats that are supported:
1. Legacy Bundles (tgz files)
2. Apt repo deb packages (.txt files)

## Request

| Field           | Type           | Description                                                                 |
|----------------|----------------|-----------------------------------------------------------------------------|
| `collectionURL`| string         | URL to a collection file (HTTP/HTTPS). Must return a valid profile.    |
| `staging`      | bool           | Reserved for future use; currently ignored.                                |

If the specified collectionURL references a .json file, the collection file will be treated as a legacy "bundles" collection and processed via calls to InstallBundle.

Sample .json bundles collection: [samples/collection-example.json](samples/collection-example.json)

On the other hand, if the specified collectionURL references a .txt file, the collection file will be handled using apt and assumed to be a simple list of deb packages.
Sample .txt debs collection: [samples/collection-example.txt](samples/collection-example.txt)

## Response

| Field       | Type   | Description                                                       |
|-------------|--------|-------------------------------------------------------------------|
| `request_id`| string | Unique ID for this install run. Use with `GetBundleStatus` to poll. |

## Collection JSON format

The URL must return JSON matching the `Profile` structure:

```json
{
  "name": "my-collection",
  "version": "1.0.0",
  "summary": "Optional summary",
  "description": "Optional description",
  "bundles": [
    {
      "name": "kubeadm",
      "version": "v1.31.9",
      "installCmd": ["python", "kubeadm-install.py"],
      "installFile": "kubeadm-install.py"
    },
    {
      "name": "display-mode-selector",
      "version": "v1.67.0",
      "installCmd": ["/bin/bash", "-c", "cp displaymodeselector /usr/bin/ && chmod 755 /usr/bin/displaymodeselector"]
    },
    {
      "name": "etcd-manager",
      "version": "v1.0.3",
      "installCmd": ["./install.sh"],
      "installFile": "install.sh"
    }
  ]
}
```

### Bundle fields

| Field        | Required | Description                                                                 |
|--------------|----------|-----------------------------------------------------------------------------|
| `name`       | yes      | Bundle name (must exist in the agent's bundle index).                       |
| `version`    | yes      | Semver string (e.g., `v1.31.9`). Must exist in the index for the node arch. |
| `installCmd` | no       | Override the default install command. Array of strings (executable + args).|
| `installFile`| no       | Override the default install script (e.g., `install.sh`).                   |

## How it works

1. The format is determined by the `collectionURL` file suffix: `.json` or `.txt`.
2. **`.json`**: The agent fetches the collection, parses it as a Profile, and installs each bundle asynchronously via `InstallBundle`.
3. **`.txt`**: The agent fetches the collection, parses it as a line-by-line list of package names, and installs via `InstallPackages` (apt-get). One package per line; lines starting with `#` and empty lines are skipped. Installation runs asynchronously; poll `GetBundleStatus(request_id)` for completion (same as `.json` collections).
4. **Status tracking**: For both formats, use `GetBundleStatus(request_id)` to track progress. For `.json` you get one status per bundle (`item_type: "bundle"`). For `.txt` you get one status (`item_type: "deb-collection"`) with the overall result.

## Examples

**Bundles (.json collection):**

```bash
grpcurl -plaintext -d '{"collectionURL": "https://example.com/collections/bootstrap.json"}' 10.85.140.130:15005 proto.NodeAgent.InstallCollection
```

**Debs (.txt collection):**

```bash
grpcurl -plaintext -d '{"collectionURL": "https://example.com/collections/packages.txt"}' 10.85.140.130:15005 proto.NodeAgent.InstallCollection
```

Response:

```json
{
  "request_id": "abc123-def456"
}
```

Poll status (works for both .json and .txt):

```bash
grpcurl -plaintext -d '{"requestId": "abc123-def456"}' 10.85.140.130:15005 proto.NodeAgent.GetBundleStatus
```

### ConfigureAptRepo (grpcurl)

`ConfigureAptRepo` installs the signing key, writes **`/etc/apt/sources.list.d/<keyring_name>.list`** (unless **`sourcesListBasename`** overrides the filename), optionally writes an apt preferences pin, and runs **`apt-get update`**. Field names use **camelCase**.

- **`repoSigningKey`**: armored ASCII or binary key material; use **`jq --rawfile`** so it is JSON-safe.
- **`keyringName`**: basename only (no `.gpg`); keyring path is `/etc/apt/keyrings/<keyringName>.gpg` and is referenced in the generated **`deb`** line.
- **`debUri`**, **`suite`**: required. Example: `debUri` = `https://nke-dev.s3.us-west-2.amazonaws.com/nke-apt-dev`, **`suite`** = `noble`.
- **`components`**: optional; default **`main`** if omitted or empty.
- **`repoName`**: when **`repoPriority`** is non-zero, must match repository **`Origin`** from `InRelease`/`Release`. When **`repoPriority`** is `0`, no preferences file is written.
- **`sourcesListBasename`**: optional; must end with `.list`. If empty, the file is **`<keyringName>.list`**.

Example: **NKE `nke-apt-dev`** — fetch the public key, configure the source and keyring, no pin (`repoPriority: 0`):

```bash
AGENT=127.0.0.1:15005
APT_ROOT='https://nke-dev.s3.us-west-2.amazonaws.com/nke-apt-dev'
curl -fsSL "${APT_ROOT}/nke-public.key" -o /tmp/nke-apt-dev.key

jq -n \
  --arg repo 'nke-apt-dev' \
  --arg keyring 'nke-apt-dev' \
  --arg debUri "$APT_ROOT" \
  --arg suite 'noble' \
  --arg components 'main' \
  --rawfile key /tmp/nke-apt-dev.key \
  '{
    repoName: $repo,
    keyringName: $keyring,
    debUri: $debUri,
    suite: $suite,
    components: $components,
    repoPriority: 0,
    repoSigningKey: $key
  }' | grpcurl -plaintext -d @- "$AGENT" proto.NodeAgent.ConfigureAptRepo
```

This creates a list file equivalent to  
`deb [signed-by=/etc/apt/keyrings/nke-apt-dev.gpg] https://nke-dev.s3.us-west-2.amazonaws.com/nke-apt-dev noble main`  
(plus a comment line). For teardown, use **`RemoveAptRepo`** with the same **`keyringName`** (and **`sourcesListBasename`** if you overrode it). If **`repoPriority`** is non-zero, set **`repoName`** to the exact **`Origin:`** from `dists/<suite>/InRelease`.

### ExecuteNodePreflight (grpcurl)

`ExecuteNodePreflight` runs optional node readiness checks and/or kubeadm preflight in
one RPC. Validation and check outcomes return **gRPC OK** with `status` set to
`ready`, `notready`, or `error` — not a gRPC error status — so orchestrators can
poll without retry storms on expected "not ready yet" results.

**Check options**

| Field | JSON | Behavior |
|-------|------|----------|
| Cloud-init status | `"checkCloudInitStatus": {}` | Runs `cloud-init status`. Passes on `status: done`. `status: running` → `notready`. `status: error` → permanent `error`. |
| File exists | `"checks": [{ "fileExists": { "path": "/path" } }]` | Passes when the absolute path exists. For legacy sentinel files or custom signals. |

At least one of `checkCloudInitStatus`, `checks`, or `kubeadmPreflight` is required.

**Default cloud-init gate** (what site-workflow-engine sends):

```bash
grpcurl -plaintext -d '{"checkCloudInitStatus": {}}' \
  127.0.0.1:15005 proto.NodeAgent.ExecuteNodePreflight
```

**Legacy sentinel file:**

```bash
grpcurl -plaintext -d '{
  "checks": [
    { "name": "sentinel", "fileExists": { "path": "/run/cloud-init.done" } }
  ]
}' 127.0.0.1:15005 proto.NodeAgent.ExecuteNodePreflight
```

**Cloud-init + kubeadm init preflight:**

```bash
grpcurl -plaintext -d '{
  "checkCloudInitStatus": {},
  "kubeadmPreflight": {
    "role": "INIT",
    "config": "<ClusterConfiguration YAML>"
  }
}' 127.0.0.1:15005 proto.NodeAgent.ExecuteNodePreflight
```

Example response when cloud-init is still running:

```json
{
  "status": "notready",
  "results": [
    {
      "name": "cloud-init-status",
      "passed": false,
      "message": "cloud-init not done: status: running"
    }
  ],
  "errorMessage": "cloud-init-status: cloud-init not done: status: running"
}
```

**site-workflow-engine behavior:** After `NodeAgentReadinessProbe` succeeds, SWE
polls every 5s for up to 10 minutes. Agents without this RPC are skipped via gRPC
reflection. If cloud-init is still `running` after the timeout, the workflow logs a
warning and continues — except when cloud-init reports `status: error`, which is a
hard failure.

## Notes

- The format is determined solely by the `collectionURL` file suffix (`.json` or `.txt`).
- For `.json`: The agent uses its configured `--bundle-server-url` and `--index-file-url` to resolve and download bundles.
- Bundles must exist in the index for the node's architecture (e.g., `amd64`, `arm64`).
- If any bundle is not found in the index, the entire request fails before any installs start.
- For `.txt`: Deb installation uses `InstallPackages` (apt-get with `--no-install-recommends`, `apt-get update` before install).
- See [samples/collection-example.json](samples/collection-example.json) and [samples/collection-example.txt](samples/collection-example.txt) for examples.


## Bundle Configuration

By default bundles are sourced from: <https://nke-dev.s3.us-west-2.amazonaws.com/nke_bundles/>

The agent supports two bundle source types via the `--bundle-server-url` flag:

### HTTP/S3 Bundle Sources (default)
Use standard HTTP/HTTPS URLs for S3 or web-hosted bundles:
```
--bundle-server-url https://nke-dev.s3.us-west-2.amazonaws.com/
```

### OCI Registry Bundle Sources
Use the `oci://` prefix to pull bundles from an OCI-compliant registry:
```
--bundle-server-url oci://nvcr.io/j7sbcjl3qgta/development
```

When using OCI registries, bundles are expected to be published as OCI artifacts with the naming convention:
- `{registry}/bundles-{name}:{version}` (e.g., `nvcr.io/j7sbcjl3qgta/development/bundles-kubeadm:v1.30.0`)

For OCI registry authentication, set the `NGC_API_KEY` environment variable.

### Index Files (HTTP/S3 only)
When using HTTP/S3 sources, the process of InstallBundle will fetch the appropriate index file (ubuntu_22_index.json or ubuntu_24_index.json)
from the BundleURL and look in the index file for the requested bundle name and version. It does this by querying 
the node for its Ubuntu version and determining which index file to use.

To make this configurable we add two startup options to the agent:
1. bundle-server-url 
2. index-override

Just setting bundle-server-url to a valid bundle source (that includes ubuntu_22_index.json and ubuntu_24_index.json)
is sufficient for normal usage of released bundles.  In cases where we might want to test staging bundles or another 
OS platform we can set up a custom index file and set the index-override variable as well.

NOTE:
We require that the index file returns a path for the bundle relative to the source URL.  So for example, if
the source url is: <https://nke-dev.s3.us-west-2.amazonaws.com/nke_bundles/>  We require bundles in the index file to 
have a path of:  nke_bundles/xxxx
