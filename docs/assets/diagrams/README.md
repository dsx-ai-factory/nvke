# Architecture diagrams

Each diagram is a hand-readable SVG with a Mermaid file beside it that holds the same structure as text. Change both together. Every node and edge is traced to code at NVKE `aaf0a6b59` and NVKR `d4c46db`.

The SVGs switch to dark colors through `prefers-color-scheme`. An SVG loaded as an image follows the reader's operating-system setting, not the site's theme toggle, so each SVG draws its own background panel and stays legible either way.

Visual key, shared by every diagram: green is NVKE, purple is NVKR, outlined is other NVIDIA software, light grey is third-party or operator-supplied, dark grey is hardware. A solid arrow is a control or data path and a dashed arrow is optional or asynchronous. Arrows point from the side that starts the interaction.

`product-family` is placed in the root `README.md` and in `docs/overview/what-is-nvke.md`. The others are not placed yet. The suggestions below follow the docs skill: one sentence above each diagram, and the acronyms spelled out in the caption.

## product-family

- **Placed in:** `docs/overview/what-is-nvke.md` under "How NVKE works", where it replaced the Mermaid diagram, and the root `README.md` under "Overview".
- **Lead-in:** The diagram shows the NVKE product family, how it sits on NICo and your GPU hardware, and which components talk to each other.
- **Caption:** NVIDIA Kubernetes Engine (NVKE), NVIDIA Kubernetes Recipes (NVKR), NVIDIA Infra Controller (NICo), the Cluster API provider for NICo (CAPNICo), NVIDIA AI Cluster Runtime (AICR).
- **Alt text:** NVKE product family. NVKR builds and signs artifacts and publishes them to a registry. The operator installs the management cluster's components, the NVKE management plane, CAPNICo and NVKE observability, from the charts and images in the registry. The management plane requests machines through CAPNICo, which calls NICo, and drives each workload cluster. NICo provisions NVIDIA GPU machines from the node OS image, and those machines become the workload cluster's Kubernetes nodes, each running nvke-agent, under the cluster's Kubernetes control plane. The accelerated runtime produced by AICR and NVKE observability are optional in the workload cluster. The customer uses kubectl against the control plane.

## nvkr-release-flow

- **Page:** `docs/operate/build-node-images.md`, under "Get the artifacts from NVKR".
- **Lead-in:** The diagram shows how NVKR turns your inputs into a Release and where each part of the Release is used.
- **Caption:** NVIDIA Kubernetes Recipes (NVKR), NVIDIA Infra Controller (NICo). Qualification is planned and not available at preview.
- **Alt text:** NVKR release flow. The Catalog, Definition and Environment compile to a Plan, which runs as a signed hosted Run in GitHub Actions or an unsigned local Run. A hosted Run publishes a Release to the registry. You verify the Release and export its release description, which the management cluster install reads to pull charts and images by digest. NICo provisions workload cluster nodes from the node OS image. A local Run directory can feed the install directly.

## management-plane-topology

- **Page:** `docs/operate/plan-installation.md`, under "What you end up with".
- **Lead-in:** The diagram shows what runs on the management cluster, the install step that puts each part there, and the connections that cross each trust boundary.
- **Caption:** NVIDIA Infra Controller (NICo), the Cluster API provider for NICo (CAPNICo), SPIFFE (Secure Production Identity Framework for Everyone) and its SPIRE runtime, JSON Web Token (JWT), mutual TLS (mTLS).
- **Alt text:** Management plane topology. The operator calls the NVKE controller on the management cluster; an optional identity provider issues the JWTs it checks. The controller uses Temporal and PostgreSQL, registers node identities with the SPIRE server, and writes objects that Cluster API and CAPNICo reconcile; CAPNICo calls NICo. The controller dials nvke-agent on each workload cluster node over gRPC with SPIFFE mTLS, and the node's SPIRE agent attests to the SPIRE server with its TPM. Badges mark the install step: nvkectl bootstrap site, clusterctl init, or nvke-deploy.sh.

## workload-cluster-provisioning (overview) and workload-cluster-provisioning-detailed

Use one, depending on how much of the internals the page should show. The overview names only the management plane; the detailed one shows Temporal, Cluster API and the SPIRE server.

- **Page:** `docs/workload-clusters/create.md`, as a "What happens when you create a cluster" section after "Watch it come up".
- **Lead-in:** The diagram shows the order of operations from your create request to a cluster your customer reaches with `kubectl`.
- **Caption:** NVIDIA Infra Controller (NICo), the Cluster API provider for NICo (CAPNICo), Container Network Interface (CNI). The detailed variant adds SPIFFE (Secure Production Identity Framework for Everyone) and its SPIRE runtime.
- **Alt text (overview):** Workload cluster provisioning. The operator creates a cluster and the management plane returns status Provisioning. The management plane requests machines from NICo through CAPNICo; NICo boots each from the node OS image; each node attests with its TPM. The management plane has nvke-agent run kubeadm init and install the CNI, marks the cluster Provisioned and joins the remaining nodes. The operator gets the kubeconfig and gives it to the customer, who uses kubectl.
- **Alt text (detailed):** Workload cluster provisioning, detailed. The NVKE controller stores the cluster and returns 201, then starts the ProvisionCluster workflow, which applies the Cluster and NicoCluster objects. For each node a ProvisionNode workflow writes a NicoMachine and Machine; CAPNICo asks NICo for an instance, the node boots and attests to the SPIRE server, and the workflow runs preflight through nvke-agent. The workflow runs kubeadm init and installs the CNI, marks the cluster Provisioned and joins the other nodes. The controller stores the admin kubeconfig as a Secret, which the operator reads and gives to the customer.
