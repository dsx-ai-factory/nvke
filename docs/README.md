# docs

Everything written down about NKE, grouped by what you are trying to do. Some of
it lives beside the code it describes rather than in this directory, so this page
indexes both.

## Start here

| Document | Contents |
|---|---|
| [overview.md](overview.md) | What NKE is and the problem it solves |
| [dsx-stack.md](dsx-stack.md) | Where NKE sits in the DSX component ecosystem (NICo, AICR, CRE, Nodewright, NVSentinel) |
| [concepts.md](concepts.md) | The terms the rest of the documentation uses |
| [first-cluster.md](first-cluster.md) | Three paths to a running cluster, driven by `nkectl` |
| [lifecycle.md](lifecycle.md) | Day 0 → Day 1 → Day 2 operational lifecycle |
| [architecture.md](architecture.md) | The modules, how a cluster request flows through them, and what is deployed where |

## Working on the code

| Document | Contents |
|---|---|
| [getting-started.md](getting-started.md) | Local development setup, and building and testing each module |
| [development.md](development.md) | The checks to run before opening a pull request, and which ones a laptop cannot reproduce |
| [go-workspace.md](go-workspace.md) | The Go workspace, which modules are in it, and how to keep `go.work.sum` current |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | How to get a change reviewed and merged |
| [../.github/workflows/README.md](../.github/workflows/README.md) | How CI is wired, and the invariants a new workflow has to hold to |

## Standing up a site

| Document | Contents |
|---|---|
| [site-bootstrap/overview.md](site-bootstrap/overview.md) | How a site controller is bootstrapped |
| [site-bootstrap/installation.md](site-bootstrap/installation.md) | Downloading and installing `nkectl` |
| [site-bootstrap/configuration.md](site-bootstrap/configuration.md) | The site spec file that describes a site |
| [site-bootstrap/pre-provisioned-nodes.md](site-bootstrap/pre-provisioned-nodes.md) | Using hosts that are already provisioned |
| [site-bootstrap/custom-base-os.md](site-bootstrap/custom-base-os.md) | Running on a base OS other than the reference image |
| [site-bootstrap/deploy-repository.md](site-bootstrap/deploy-repository.md) | The deploy repository and what it holds |
| [site-bootstrap/infrastructure-providers.md](site-bootstrap/infrastructure-providers.md) | Installing Cluster API and an infrastructure provider on a site cluster |
| [../cli/docs/sitespec.md](../cli/docs/sitespec.md) | Every field of the site spec file, with its type |

## Running tenant clusters

| Document | Contents |
|---|---|
| [tenant-clusters/node-lifecycle.md](tenant-clusters/node-lifecycle.md) | What happens to a node through its life |
| [tenant-clusters/kubernetes-api-access.md](tenant-clusters/kubernetes-api-access.md) | How operators, automation and Site Controller services reach and authenticate to tenant Kubernetes APIs |
| [../controller/docs/authenticating-to-the-api.md](../controller/docs/authenticating-to-the-api.md) | Authenticating to the API |
| [../controller/docs/configuring-api-authentication.md](../controller/docs/configuring-api-authentication.md) | Turning API authentication on, including RBAC for `nkectl` |
| [../controller/docs/byo-os.md](../controller/docs/byo-os.md) | Bringing your own OS image |

## Operating a site

| Document | Contents |
|---|---|
| [operations/upgrade.md](operations/upgrade.md) | Upgrading NKE and tenant cluster Kubernetes versions |
| [operations/observability.md](operations/observability.md) | Temporal UI, CWE logs, database health, and CAPNICo diagnostics |
| [operations/faq.md](operations/faq.md) | Common bootstrap and tenant cluster failure modes |

## The modules

Each module has its own README. `go.work` is the list of record for which modules
exist.

| Module | Document |
|---|---|
| Cloud Workflow Engine | [../controller/README.md](../controller/README.md) |
| Node agent | [../agent/README.md](../agent/README.md) |
| `nkectl` | [../cli/README.md](../cli/README.md) |
| Generated API | [../controller/api/README.md](../controller/api/README.md) |
| `nico-sim` VM and container simulator | [../controller/nico-sim/README.md](../controller/nico-sim/README.md) |
| Reference node OS image | [../os-image/README.md](../os-image/README.md) |
| Cluster API provider | [../cluster-api-provider-provisioned/README.md](../cluster-api-provider-provisioned/README.md) |
| CAPD reboot test helper | [../controller/hack/capd-reboot-controller/README.md](../controller/hack/capd-reboot-controller/README.md) |

Deeper module documentation:

| Document | Contents |
|---|---|
| [../agent/docs/DESIGN.md](../agent/docs/DESIGN.md) | The node agent's architecture and API surface |
| [../agent/docs/SECURITY.md](../agent/docs/SECURITY.md) | The node agent's security context |
| [../os-image/SPEC.md](../os-image/SPEC.md) | The contract a node image has to meet |
| [../controller/profiles/README.md](../controller/profiles/README.md) | External profiles |
| [../controller/deploy/charts/controller/README.md](../controller/deploy/charts/controller/README.md) | The engine's Helm chart and its values |
| [../nktp/deploy/charts/nktp/README.md](../nktp/deploy/charts/nktp/README.md) | The NKTP Helm chart, what it deploys and its values |
| [../build/README.md](../build/README.md) | The per-component build and publish scripts |
| [../os-image/README.md](../os-image/README.md) | Node image build, FDE/non-FDE variants, release |

## Tests

| Document | Contents |
|---|---|
| [../controller/test/e2e/README.md](../controller/test/e2e/README.md) | The end to end suite |
| [../controller/integration-test/README.md](../controller/integration-test/README.md) | The integration tests |
| [../controller/nico-sim/docs/e2e-testing.md](../controller/nico-sim/docs/e2e-testing.md) | End-to-end testing for nico-sim's VM backend |
| [nktp/kind-validation.md](nktp/kind-validation.md) | Ad hoc validation of the NKTP chart's default values on a Kind cluster |

## Project and process

| Document | Contents |
|---|---|
| [../README.md](../README.md) | The repository overview |
| [../SECURITY.md](../SECURITY.md) | Reporting a vulnerability |
| [../SUPPORT.md](../SUPPORT.md) | Support channels |
| [../GOVERNANCE.md](../GOVERNANCE.md) | How the project is governed |
| [../MAINTAINERS.md](../MAINTAINERS.md) | Who maintains what |
| [../CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md) | Expected conduct |
| [../RELEASE.md](../RELEASE.md) | How a release is cut |
| [../CHANGELOG.md](../CHANGELOG.md) | Changes not yet released |

## For coding agents

`AGENTS.md` files carry instructions for AI coding agents, not for people. There
is one at the repository root, and one in
[../agent/AGENTS.md](../agent/AGENTS.md),
[../controller/AGENTS.md](../controller/AGENTS.md),
[../nktp/AGENTS.md](../nktp/AGENTS.md) and
[../os-image/AGENTS.md](../os-image/AGENTS.md).

## Not indexed here

Left out on purpose, because they are machine written or scoped to one piece of
work rather than to a reader:

- the generated `THIRD_PARTY_NOTICES.md` files, and the bundles under `generated/`
- the working notes under `controller/hack/bgp-containers/`
- package-level notes that sit beside the code they describe, such as
  `controller/internal/rollout/README.md`. They are for whoever is editing that
  package, and indexing every one of them here would bury the documents that are
  written for a reader.
