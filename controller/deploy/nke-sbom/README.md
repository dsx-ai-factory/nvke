# nke-sbom

The deploy path for a NICo site controller on real hardware, with SPIRE.

Not a reference implementation: the item list and its order are derived from a
provisioning run that stood a site controller up on bare metal and had it
provision a tenant. Where that run and `capnico-e2e` disagree, this path follows
the run -- it is the one that has been exercised on real hardware.

## Where this sits

Four steps, three owners:

| step | who | produces |
|---|---|---|
| 1. create and image the instance | **not NKE** -- the NICo API, iPXE and cloud-init | a booted host running `nke-agent` |
| 2. `nkectl bootstrap site` | nkectl, on the instance | a **vanilla** Kubernetes cluster |
| 3. this deploy path | `use-nke-sbom.sh`, on the node | the management plane |
| 4. `nkectl create cluster` | the site controller itself | tenant clusters |

Step 2 leaves bare Kubernetes on purpose. `bootstrap site` stands up a
throwaway kind cluster carrying Postgres, Temporal, SPIRE, CAPI and CWE, uses
that CWE to drive `kubeadm init` on the host over the node agent, and then
deletes it -- taking its record of the cluster with it. That is why a site
controller cannot later be scaled or upgraded through the API, and why step 3
starts from nothing.

Step 3 runs ON the node, not against it from a workstation. That is what makes
the image story cheap: the node's containerd IS the cluster's containerd, so
an image imported there resolves with `imagePullPolicy: IfNotPresent` and needs
no registry credential. CAPNICo is the one exception, and the one pull secret.

What the node needs, all present on the reference instance type:

- `helm`, `kubectl`, `git`, `python3` (the executor's item parser and template
  renderer are Python), plus `clusterctl` for the prerequisites
- a checkout laid out so that `deploy/` and `controller/` are siblings --
  `controller/release.yaml` reaches its chart as
  `../../controller/deploy/charts/controller`
- egress to `registry-1.docker.io` (the Bitnami OCI chart),
  `spiffe.github.io` and `go.temporal.io` (the spire-crds and temporal
  repositories), and GitHub if the spire chart is materialised there

## Before this runs

`use-nke-sbom.sh` installs the items below and nothing else. Three things must
already be true, all of them outside the deploy path:

1. **The control plane is untainted.** A bootstrap cluster is all
   control-plane and tainted `NoSchedule`. Nothing schedules until it is
   removed, and `clusterctl init` times out on cert-manager with no hint why.
2. **`clusterctl init` has run**, bringing cert-manager, CAPI core and the
   kubeadm providers. cert-manager first because CAPI's manifests embed an
   Issuer and a Certificate.
3. **The CWE and profiles images are in the node's containerd.** The
   controller overlay sets `imagePullPolicy: IfNotPresent` and this path
   supplies no pull secret for them -- deliberately, because the node's
   containerd IS the cluster's containerd, so an imported image resolves
   without one. Import them before installing:

   ```
   docker save --platform linux/amd64 <image> | sudo ctr -n k8s.io images import -
   ```

   Namespace `k8s.io`, not `default`: an image imported into the wrong
   containerd namespace is invisible to the kubelet, which that run
   hit as "401 Unauthorized pulling the controller image". Build
   single-manifest too (`--provenance=false --output type=docker`) -- an OCI
   index with an attestation manifest made nkectl try to pull by digest.

4. **A default StorageClass exists.** A bare site ships none and every PVC sits
   Pending. The reference site used rancher local-path because its instance type has a single
   NVMe with root on it; a site with real storage should use that instead.

## After this runs

**Inject the SPIRE exec-auth plugin into the CAPI controllers**, with
`controller/hack/apply-capi-spiffe-exec-auth.sh`.

`controller/deploy/k8s/capi-spiffe-exec-auth/` patches the CAPI controller
Deployments with a `csi.spiffe.io` volume and the SPIFFE JWT exec-auth plugin,
so Cluster API reaches tenant clusters with a SPIFFE JWT rather than a stored
admin kubeconfig. Both e2e lanes run it -- `backend-e2e.sh` does so right after
the deploy path, on the NICo lane as well as CAPD -- and a site with
`spiffe.enabled=true` needs it for the same reason.

It is not an item, and cannot be: it patches Deployments `clusterctl init`
owns, which this deploy path does not install. It belongs with untaint and
`clusterctl init`, except that it runs AFTER rather than before -- the script
skips its rollout wait until `csi.spiffe.io` is registered, and that CSIDriver
arrives with the spire item. Run before the install it patches and moves on,
leaving the rollout unverified; run after, it is checked.

The overlay includes `capd-controller-manager.yaml`, which a NICo site has no
namespace for. The script skips Deployments that are absent, so this is fine.

That run's install script does none of this, because it had
`spiffe.enabled=false` and nothing needed a workload API.

## Per-site values

Every item's committed values are site-independent. What varies comes in
through `--values-for`, exactly as `controller/hack/ci/deploy-sbom.sh` does it
for the e2e lanes -- that script is the reference for these derivations.

| value | e2e derivation | on a NICo site |
|---|---|---|
| trust domain | `${KIND_CLUSTER_NAME}.nke.local` | `<site-name>.nke.local` |
| management VIP | kind control-plane IP | the site controller's node IP |
| SPIRE admin path | `/nke-service/cloud-workflow-engine` | the same; it is a constant |
| TPM attestor **server** checksum | pinned per arch in `resolve_tpm_checksum()` | the same pin for the same arch |
| OIDC domains / IP SANs | VIP plus the kind gateway | the VIP |
| discovery URL | `https://<VIP>:30443/.well-known/openid-configuration` | the same shape |
| Postgres / Temporal password | generated per run | site secret, on BOTH Temporal datastores |
| controller image | built per run | a released tag |
| profiles image | built per run | the site's fleet profile content |

The TPM checksum is easy to confuse: the value in `spire/values.yaml`
(`spire-server.nodeAttestor.tpmDirect.checksum`) is the **server** plugin, and
is unrelated to `TPM_PLUGIN_CHECKSUM` in `/etc/spire-agent/spire-agent.env` on
a node, which is the **agent** plugin. Both are per-architecture.

## SPIRE

`spiffe.enabled=true`, and the five SPIRE items are the same ones
`capnico-e2e` installs and proves on every run.

Node-side needs nothing built: the OS profile writes
`/etc/spire-agent/agent.conf` and `initial-trust-bundle.pem` into cloud-init,
which is one of the two mutually exclusive paths `os-image/SPEC.md` allows --
the other being `/etc/nke/spire.env` plus the image's renderer. A profile that
takes the first path works on an image that lacks the second.

Two ordering facts worth knowing before the first run, neither of which any
lane has exercised on NICo:

- CWE fills `initial-trust-bundle.pem` from its own SPIFFE workload socket, so
  **CWE must hold an SVID before it can bootstrap a node**.
- Tenant nodes must reach the SPIRE server at the address the profile renders.
  In e2e that is a kind NodePort; here it is the VIP, published by the
  `spire-server-nodeport` item.

The site controller's own node is a separate matter and stays plaintext: it is
provisioned outside CWE, and when it bootstraps the only SPIRE alive is the one
in the throwaway kind cluster, which is deleted. Nothing dials that node
afterwards.
