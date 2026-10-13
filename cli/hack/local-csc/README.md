# local-csc

A local, single-node CSC (site controller) for development on a Mac. One
container stands in for a pre-provisioned host running `nke-agent`, and
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
cd /Users/$USER/dev/oss/nke   # the repository, mounted at its macOS path (the VM's ~ differs)
cli/hack/local-csc/local-csc.sh build    # first time, and after code changes
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
| `K8S_VERSION` | `1.34.8` | must match the node image's kubeadm |
| `REBUILD_NODE_IMAGE` | unset | `1` makes `build` rebuild the node image, e.g. after an agent change |

## Why it looks like this

- **nvkectl runs inside the VM.** `bootstrap site` needs rootful nerdctl, and it
  saves image archives that kind reads back. A host-side nerdctl shim breaks
  both.
- **The node is configured by an entrypoint, not a derived image.**
  [`node-init.sh`](node-init.sh) runs before systemd. BuildKit in the Colima VM
  cannot build `FROM` a nerdctl-local image. Patching after boot is too late:
  `nke-container-network.service` moves the uplink into OVS on first boot.
- **`up` may requeue the Machine.** CAPI can look up the Node a second before
  the provisioned provider sets its providerID, and then never look again. `up`
  and `import` annotate the Machine to requeue it.
- **`import`** uses the provisioned provider's preset-`providerID` path. It
  copies the Cluster, ProvisionedCluster, Machine, ProvisionedMachine and their
  Secrets, and sets `controlPlaneEndpoint` so the Cluster reaches `Provisioned`.
  Never `kubectl delete` the imported objects: deleting a Machine starts CAPI's
  node deletion against the CSC.
