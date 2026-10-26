# local-csc

A local, single-node CSC (site controller) for development on a Mac. One
container stands in for a pre-provisioned host running `nvke-agent`, and
`nvkectl bootstrap site --disable-mtls` turns it into a kubeadm cluster. The
bootstrap kind cluster is kept, so the CSC's Cluster API objects stay
inspectable.

This is a development path only: no SPIRE, no TPM, plaintext agent gRPC.

## Usage

```bash
# On the Mac: create the Colima VM and install the pinned tools in it
cli/hack/local-csc/local-csc.sh vm-up

# In the VM
colima ssh --profile nvke
tmux new -s csc
cd /Users/$USER/<checkout>   # this repository, mounted at its macOS path (the VM's ~ differs)
cli/hack/local-csc/local-csc.sh build    # first time (it builds OVN-Kubernetes from source), and after code changes
cli/hack/local-csc/local-csc.sh up       # node + bootstrap, 10-20 minutes
cli/hack/local-csc/local-csc.sh status
cli/hack/local-csc/local-csc.sh import   # optional: adopt the CSC into a fresh CAPI management cluster
cli/hack/local-csc/local-csc.sh down
```

`kubectl` for the CSC, from inside the VM:

```bash
sudo kubectl --kubeconfig ../nvke-dev/csc-local.kubeconfig get nodes
```

After a VM or Mac restart the CSC does not survive (its etcd is on tmpfs). Run
`down`, then `up`.

## Configuration

| Variable | Default | |
|---|---|---|
| `STATE_DIR` | `../nvke-dev` beside the repository | site spec, kubeconfig, etcd key, logs. Holds credentials; keep it out of the repository. |
| `COLIMA_PROFILE` | `nvke` | `VM_CPUS`, `VM_MEMORY`, `VM_DISK` size it on first creation |
| `CLUSTER_NAME` | `csc-local` | |
| `NODE_NAME`, `NODE_IP` | `csc-node-0`, `10.4.1.10` | `NODE_IP` must be inside the `kind` network's subnet |
| `NODE_COUNT` | `1` | hosts to start, `csc-node-<i>` at `NODE_IP` + i. Only host 0 is bootstrapped: `bootstrap site` needs a `controlPlaneVIP` for more, and DSX CSCs have none. Join the rest out of band. |
| `IMPORT_TARGET` | `kind` | `csc` makes `import` install Cluster API on the CSC itself and import there (self-managed) instead of into a fresh kind cluster |
| `MGMT_NAMESPACE` | `nvke-system` | namespace `import` puts the CSC's CAPI objects in. A dedicated one lets RBAC and admission policy protect them as a whole. |
| `IMPORT_EXTRA` | unset | for `import`: a JSON file of inventory hosts and Machine objects for out-of-band hosts. Their Machines are created first, so the endpoint holder's is the newest and KCP updates it last. |
| `K8S_VERSION` | `1.34.8` | must match the node image's kubeadm |
| `REBUILD_NODE_IMAGE` | unset | `1` makes `build` rebuild the node image, e.g. after an agent change |

## Why it looks like this

- **nvkectl runs inside the VM.** `bootstrap site` needs rootful nerdctl, and it
  saves image archives that kind reads back. A host-side nerdctl shim breaks
  both.
- **The node is configured by an entrypoint, not a derived image.**
  [`node-init.sh`](node-init.sh) runs before systemd. BuildKit in the Colima VM
  cannot build `FROM` a nerdctl-local image, and the agent's flags must be set
  before it first starts.
- **OVN-Kubernetes, built in the VM.** `bootstrap site` accepts only
  OVN-Kubernetes. `build` makes its packages the way CI's container nodes do
  (`controller/hack/ci/prepare-ovn-assets.sh`, with nerdctl as the builder) and
  bakes them into the node image. The pod network is `10.244.0.0/16`: the site
  spec's default, `192.168.0.0/16`, would take in Colima's VM network
  (`192.168.5.0/24`, where the VM's DNS answers).
- **nerdctl's networks live in `/etc/containerd-nerdctl/net.d`.** nvkectl refuses
  to bootstrap while nerdctl network configs are in `/etc/cni/net.d`, the
  directory a site node's containerd reads, and points its own kind commands at
  `/etc/containerd-nerdctl/net.d`. `provision` sets `cni_netconfpath` in
  `/etc/nerdctl/nerdctl.toml` and moves existing configs there, so every
  nerdctl in the VM uses the same `kind` network. On an existing VM, run
  `provision` again.
- **`up` may requeue the Machine.** CAPI can look up the Node a second before
  the provisioned provider sets its providerID, and then never look again. `up`
  and `import` annotate the Machine to requeue it.
- **`import`** uses the provisioned provider's preset-`providerID` path. It
  copies the Cluster, ProvisionedCluster, Machine, ProvisionedMachine and their
  Secrets, and sets `controlPlaneEndpoint` so the Cluster reaches `Provisioned`.
  Never `kubectl delete` the imported objects: deleting a Machine starts CAPI's
  node deletion against the CSC.
