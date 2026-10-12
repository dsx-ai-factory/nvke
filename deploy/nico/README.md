# deploy/nico

The deploy path for a NICo site controller on real hardware, with SPIRE. It is
the customer deploy path; the e2e lanes under `controller/deploy/` are not.

Not a reference implementation: the item list and its order are derived from a
provisioning run that stood a site controller up on bare metal and had it
provision a tenant. Where that run and `capnico-e2e` disagree, this path follows
the run -- it is the one that has been exercised on real hardware.

## Where this sits

Four steps, three owners:

| step | who | produces |
|---|---|---|
| 1. create and image the instance | **not NVKE** -- the NICo API, iPXE and cloud-init | a booted host running `nvke-agent` |
| 2. `nvkectl bootstrap site` | nvkectl, on the instance | a **vanilla** Kubernetes cluster |
| 3. this deploy path | `deploy/nvke-deploy.sh`, on the node | the management plane |
| 4. `nvkectl create cluster` | the site controller itself | tenant clusters |

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
- to render the site's values first: `nvkectl` (`make -C cli build` writes
  `cli/bin/nvkectl`). `nvkectl render deploy-path --deploy-path deploy/nico
  --sitespec sitespec.yaml --out DIR`, with `DIR` under a directory only you
  can read such as `~/.local/state/nvke/renders/<site>` (a shared temp
  directory is refused), writes a copy of this deploy path with the site's
  values filled in from the spec's `controllerStack` section. `DIR` holds
  the site's secrets, so it is created `0700` and is for one install. The
  command ends by naming the files under `DIR` that hold a credential;
  delete `DIR` after the install. `DIR` gets a `.gitignore` that ignores
  everything, so a repository does not pick it up by accident. Then install
  `DIR` with this script; it needs no `--config`. Without a release, the
  spec sets `controllerStack.images.profiles`: this deploy path enables the
  profiles content image without naming it
- a checkout laid out so that `deploy/` and `controller/` are siblings --
  `controller/release.yaml` reaches its chart as
  `../../controller/deploy/charts/controller`
- egress to `registry-1.docker.io` (the Bitnami OCI chart),
  `spiffe.github.io` and `go.temporal.io` (the spire-crds and temporal
  repositories), and GitHub if the spire chart is materialised there
- for the nktp item, egress to `open-telemetry.github.io`,
  `prometheus-community.github.io` and `kubernetes-sigs.github.io`, where the
  chart's dependencies come from, and to `docker.io`, `ghcr.io`, `quay.io` and
  `registry.k8s.io`, where its images come from
- to install from an NVKR release: `nvkectl` on the machine that renders.
  `nvkectl render deploy-path --deploy-path deploy/nico --release
  release.json --out DIR` resolves this deploy path against the
  `nvkr.dev/release/v4` JSON that `nvkr release verify` prints. Charts the
  release carries are pulled by digest into `DIR/charts/`; images are pinned
  by digest; a chart the release lacks stays on its public repository. Each
  item's `release.yaml` names its artifacts: `chart.artifact` (default: the
  item name), `chart.imageArtifact`, `chart.profilesArtifact`, or a manifest
  item's `artifact`. `--arch` selects them (default: the site spec's
  `controllerStack.arch`). The render refuses, naming every problem, when an
  image artifact is missing. Then install `DIR` with this script; it needs
  no `nvkectl`.

## Before this runs

`deploy/nvke-deploy.sh` installs the items below and nothing else. Five things must
already be true, all of them outside the deploy path:

1. **The control plane is untainted.** A bootstrap cluster is all
   control-plane, and nothing schedules while the `NoSchedule` taint is on
   the nodes; `clusterctl init` times out on cert-manager with no hint why.
   `nvkectl bootstrap site` removes the taint. Check with
   `kubectl get nodes -o jsonpath='{.items[*].spec.taints}'` and remove it by
   hand if the bootstrap reported that it could not.
2. **`clusterctl init` has run**, bringing cert-manager, CAPI core and the
   kubeadm providers. cert-manager first because CAPI's manifests embed an
   Issuer and a Certificate.
3. **The CWE and profiles images are in the node's containerd.** The
   controller overlay sets `imagePullPolicy: IfNotPresent` and this path
   supplies no pull secret for them -- deliberately, because the node's
   containerd IS the cluster's containerd, so an imported image resolves
   without one. A deploy path rendered against a release sets the two images
   by digest from the release's `cwe` and `controller-profiles` artifacts,
   and the CAPNICo image from `capnico-controller`; the nodes then pull them
   from the registries the release names, so those registries need to be
   reachable from the nodes, with a containerd registry config or a pull
   secret where they need one. Rendered with `--nvkr-run-dir` as well, from a
   completed NVKR Run (`.nvkr/runs/<attempt>`, whose `packages/` holds one OCI
   layout per artifact), nothing is pulled: charts come out of the layouts,
   and `images.txt` in the rendered path names each image's layout, which
   this script imports into the node's containerd under the reference's name
   before applying anything, so the reference's host is only a name. `CTR`
   is the import command and may carry arguments: `CTR="sudo ctr"` when the
   install does not run as root, `CTR="docker exec -i kind-control-plane
   ctr"` when the cluster is a kind node. Without a render, import the
   images before installing:

   ```
   docker save --platform linux/amd64 <image> | sudo ctr -n k8s.io images import -
   ```

   Namespace `k8s.io`, not `default`: an image imported into the wrong
   containerd namespace is invisible to the kubelet, which that run
   hit as "401 Unauthorized pulling the controller image". Build
   single-manifest too (`--provenance=false --output type=docker`) -- an OCI
   index with an attestation manifest made nvkectl try to pull by digest.

4. **A default StorageClass exists.** A bare site ships none and every PVC sits
   Pending. The reference site used rancher local-path because its instance type has a single
   NVMe with root on it; a site with real storage should use that instead.
5. **The cluster has no install from an earlier revision of this path.** This
   path installs the controller as release `nke-controller`, with database
   `nke-controller` and Temporal namespace `nke-controller`. Earlier revisions
   used release `controller`, database `cloud-workflow` and the chart's
   Temporal names. Helm cannot take over the old release's objects, and
   Postgres runs its `initdb` scripts only on an empty volume. Nothing
   migrates the old database: install on a fresh cluster.
6. **The site's release CRDs and resources are installed.** CWE reads its OS
   images, user init scripts and so on from `MachineImage`, `ClusterRelease`
   and `NodePoolRelease` resources. The chart installs the CRDs. From a second
   shell, once the CRDs exist, run
   `kubectl apply --server-side -f <the site's releases directory>`.

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
through `--values-for`, exactly as `controller/hack/ci/deploy-stack.sh` does it
for the e2e lanes -- that script is the reference for these derivations.

| value | e2e derivation | on a NICo site |
|---|---|---|
| trust domain | `${KIND_CLUSTER_NAME}.nke.local` | the trust domain the site's nodes were bootstrapped with (`spire.trustDomain` in the site spec); never derived from a name |
| management VIP | kind control-plane IP | the site controller's node IP |
| SPIRE admin path | `/nke-service/nke-controller` | the same; it is a constant |
| TPM attestor **server** checksum | pinned per arch in `resolve_tpm_checksum()` | the same pin for the same arch |
| OIDC domains / IP SANs | VIP plus the kind gateway | the VIP |
| discovery URL | `https://<VIP>:30443/.well-known/openid-configuration` | the same shape |
| Postgres / Temporal password | generated per run | site secret, on BOTH Temporal datastores |
| controller image | built per run | a released tag |
| profiles image | built per run | the site's fleet profile content |
| telemetry backend | not installed | the bundled development backend, until the site passes its own with `--values-for nktp=` |
| telemetry `k8s.cluster.name` | not installed | `ClusterName` in the BootstrapConfig passed with `--config`; required |

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

## Telemetry

The `nktp` item installs the NKTP chart from this checkout
(`nktp/deploy/charts/nktp`), or from the archive a render against a release
that carries the `nktp` chart artifact put under `charts/`, with the values
in `nktp/values.yaml`. It comes
straight after `capnico` because it installs the monitoring CRDs, and three
later items create monitors that need them:

| item | what it adds | where |
|---|---|---|
| `temporal` | a ServiceMonitor per server role | `temporal/overlay.yaml` |
| `spire` | PodMonitors for the server and the agent | `spire/overlay.yaml` |
| `controller` | its ServiceMonitor, a metrics NetworkPolicy that admits `nktp`, and traces to the NKTP node Collector | `controller/overlay.yaml` |

The chart's own control-plane monitors cover the API server, etcd and CoreDNS;
`nktp/values.yaml` turns off the scheduler, controller manager and kube-proxy
ones, which NVKE clusters cannot serve. This path creates no monitors for other
components: each component ships its own, and one that does not is not scraped.

Everything NKTP exports carries `k8s.cluster.name`, the site's name. Nothing
in the cluster knows it -- NVKE's kubeadm configuration names every cluster
`kubernetes` -- so `nvke-base-config` renders it from BootstrapConfig's
`ClusterName` into the `nktp-cluster` ConfigMap, and the watchdog and the three
gateways, which are what export to the backend, read it from there. A config
without `ClusterName` stops the install at that template.

Everything exports to the chart's bundled development backend by default. It
keeps data in memory and has no authentication, so it is for evaluation. A site
with a real backend passes it with `--values-for nktp=<file>`; the chart
README's "The backend" section lists the keys that file sets.

Not scraped yet: the Postgres item runs no metrics exporter; CAPI core and the
kubeadm providers serve metrics only to a caller with a bearer token, which the
node Target Allocator does not send; the scheduler and controller manager
listen on 127.0.0.1 only; and cert-manager, NodeLocal DNSCache,
OVN-Kubernetes and CAPNICo are installed without a monitor. Their pods' state still arrives through
kube-state-metrics.
