# deploy/nico

The deploy path for an NVKE management cluster on NICo-managed hardware, with SPIRE. It is
the customer deploy path; the e2e lanes under `controller/deploy/` are not.

Not a reference implementation: the item list and its order are derived from a
provisioning run that stood a management cluster up on bare metal and had it
provision a tenant. Where that run and `capnico-e2e` disagree, this path follows
the run -- it is the one that has been exercised on real hardware.

## Where this sits

Four steps, three owners:

| step | who | produces |
|---|---|---|
| 1. create and image the instance | **not NVKE** -- the NICo API, iPXE and cloud-init | a booted host running `nvke-agent` |
| 2. `nvkectl bootstrap site` | nvkectl, on the instance | a **vanilla** Kubernetes cluster |
| 3. this deploy path | `deploy/nvke-deploy.sh`, on the node | the management plane |
| 4. `nvkectl create cluster` | the management plane itself | tenant clusters |

Step 2 leaves bare Kubernetes on purpose. `bootstrap site` stands up a
throwaway kind cluster carrying Postgres, Temporal, SPIRE, CAPI and CWE, uses
that CWE to drive `kubeadm init` on the host over the node agent, and then
deletes it -- taking its record of the cluster with it. That is why a site
controller cannot later be scaled or upgraded through the API, and why step 3
starts from nothing.

Step 3 runs ON the node, not against it from a workstation. That is what makes
the image story cheap: the node's containerd IS the cluster's containerd, so
an image imported there resolves with `imagePullPolicy: IfNotPresent` and needs
no registry credential, and CAPNICo's image is public on GHCR or comes from
the release by digest, so this path renders no pull secret.

What the node needs, all present on the reference instance type:

- `helm`, `kubectl`, `git`, `python3` (the executor's item parser and template
  renderer are Python), plus `clusterctl` for the prerequisites
- to install for a site: `nvkectl` (`make -C cli build` writes
  `cli/bin/nvkectl`; `NVKECTL` names it, else PATH). `deploy/check-site.sh --sitespec
  sitespec.yaml --stack-config stack-config.json --kubeconfig site.kubeconfig`
  reports what is still missing; `deploy/nvke-deploy.sh install --deploy-path
  deploy/nico --sitespec sitespec.yaml --stack-config stack-config.json
  --kubeconfig site.kubeconfig` renders this deploy path for the site from
  the two files and applies it: the site spec gives the site name, VIP and
  trust domain, the stack config (`deploy/examples/stack-config.json`,
  filled in) the NICo API and the optional images and mirror. The render
  lives in a private scratch directory and is removed when the script
  exits, so no rendered credential is left behind. Without a release, the
  stack config sets `images.profiles`: this deploy path enables the
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
- to install from an NVKR release: add `--release release.json`, the
  `nvkr.dev/release-description/v1` JSON that `nvkr release export
  --reference <repo@digest>` writes. The export describes the release; it
  verifies nothing. Run `nvkr release verify` on the same reference first.
  Charts the release carries are pulled by digest; images are pinned by
  digest; a chart the release lacks stays on its public repository. The
  release decides the version of a chart it carries, with a warning when
  the item pins another; the pin decides only for a chart the release
  lacks. Each item's `release.yaml` names its artifacts: `chart.artifact`
  (default: the item name), `chart.imageArtifact`, `chart.profilesArtifact`,
  or a manifest item's `artifact`. Artifacts are selected for the
  architecture the cluster's nodes report, which the executor reads and
  passes to the render as `--arch`. The render refuses, naming every problem,
  when an image artifact is missing. With `--nvkr-run-dir
  .nvkr/runs/<attempt>`, charts and images come from the Run's OCI layouts
  and the images are imported into the node's containerd (`CTR` names the
  import command).

## Before this runs

`deploy/nvke-deploy.sh` installs the items below and nothing else. Five things must
be true before it runs and one after, all of them outside the deploy path:

1. **The control plane is untainted.** A bootstrap cluster is all
   control-plane, and nothing schedules while the `NoSchedule` taint is on
   the nodes; `clusterctl init` times out on cert-manager with no hint why.
   `nvkectl bootstrap site` removes the taint, and `deploy/check-site.sh`
   reports a node that still carries it, with the command that clears it.
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
   path installs the controller as release `nvke-controller`, with database
   `nvke-controller` and Temporal namespace `nvke-controller`. The revision
   before this one used `nke-controller` for all three, and revisions before
   that used release `controller`, database `cloud-workflow` and the chart's
   Temporal names. Helm cannot take over the old release's objects, and
   Postgres runs its `initdb` scripts only on an empty volume. Nothing
   migrates the old database: install on a fresh cluster.
6. **The site's release resources are published afterwards.** CWE reads its
   OS images, user init scripts and so on from `MachineImage`,
   `ClusterRelease` and `NodePoolRelease` resources. The chart installs the
   CRDs; after `nvkectl config set-site`, publish the resources with
   `nvkectl create machineimage|clusterrelease|nodepoolrelease -f <file>`,
   as docs/get-started/local-site.md shows.

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
owns, which this deploy path does not install. It belongs with `clusterctl init`,
except that it runs AFTER rather than before -- the script
skips its rollout wait until `csi.spiffe.io` is registered, and that CSIDriver
arrives with the spire item. Run before the install it patches and moves on,
leaving the rollout unverified; run after, it is checked.

The overlay includes `capd-controller-manager.yaml`, which a NICo site has no
namespace for. The script skips Deployments that are absent, so this is fine.

That run's install script does none of this, because it had
`spiffe.enabled=false` and nothing needed a workload API.

## Per-site values

Every item's committed values are site-independent. The install writes what
varies into each item's `site-values.yaml` from the site spec; the e2e lanes
derive the same values in `controller/hack/ci/deploy-stack.sh`. `--values-for` still applies on top of a
rendered path, after `site-values.yaml`.

| value | the install takes it from | e2e derivation |
|---|---|---|
| trust domain | `spire.trustDomain`, else `nvkectl-bootstrap.nke.local` with a warning | `${KIND_CLUSTER_NAME}.nvke.local` |
| management VIP | `cluster.controlPlaneVIP`, or the only node's IP | kind control-plane IP |
| SPIRE admin path | a constant, `/nvke-service/nvke-controller` | the same |
| TPM attestor **server** checksum | pinned per the nodes' architecture, `--arch` | pinned per arch in `resolve_tpm_checksum()` |
| OIDC domains / IP SANs | the overlay's hostnames plus the VIP | VIP plus the kind gateway |
| discovery URL | `https://<VIP>:30443/.well-known/openid-configuration` | the same shape |
| Postgres / Temporal password | `NVKECTL_DATABASE_PASSWORD`, written into the `nvke-database` Secret of the postgres, temporal and nvke-controller namespaces by the `site-credentials` item the render adds; every chart reads it from there and no values file carries it | generated per run |
| SPIRE upstream CA | `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE`, written into the `spire-upstream-ca` Secret in spire-server by the same item, when `spire.upstreamCA` is set | self-signed |
| controller image | the release, or the stack config's `images.controller` | built per run |
| profiles image | the release, or the stack config's `images.profiles` | built per run |
| telemetry backend | `--values-for nktp=<file>`, else the chart's bundled development backend | not installed |
| telemetry `k8s.cluster.name` | `clusterName`, through the `nvke-base-config` template | not installed |

The TPM checksum is easy to confuse: the value in `spire/values.yaml`
(`spire-server.nodeAttestor.tpmDirect.checksum`) is the **server** plugin, and
is unrelated to `TPM_PLUGIN_CHECKSUM` in `/etc/spire-agent/spire-agent.env` on
a node, which is the **agent** plugin. Both are per-architecture.

## SPIRE

`spiffe.enabled=true`, and the five SPIRE items are the same ones
`capnico-e2e` installs and proves on every run.

Node-side needs nothing built: the release writes
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

The management cluster's own node is a separate matter and stays plaintext: it is
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
`kubernetes` -- so `nvke-base-config` renders it into the `nktp-cluster`
ConfigMap, and the watchdog and the three gateways, which are what export to
the backend, read it from there. With `--sitespec`, the render takes it from
the spec's `clusterName`. A plain install without a site spec renders the
template from the BootstrapConfig passed with `--config`, whose `ClusterName`
must be set; an empty name stops the install at that template.

Everything exports to the chart's bundled development backend by default. It
keeps data in memory and has no authentication, so it is for evaluation. A site
with a real backend passes `--values-for nktp=<file>` to the install, which
applies it after the item's own values, with or without a site spec. The chart
README's "The backend" section lists the keys that file sets.

Not scraped yet: the Postgres item runs no metrics exporter; CAPI core and the
kubeadm providers serve metrics only to a caller with a bearer token, which the
node Target Allocator does not send; the scheduler and controller manager
listen on 127.0.0.1 only; and cert-manager, NodeLocal DNSCache,
OVN-Kubernetes and CAPNICo are installed without a monitor. Their pods' state still arrives through
kube-state-metrics.
