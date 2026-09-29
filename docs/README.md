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
| [coverage-policy.md](coverage-policy.md) | What coverage is collected, what CI does with it, and why no threshold gates a merge |
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
| [../cli/docs/bootstrap-troubleshooting.md](../cli/docs/bootstrap-troubleshooting.md) | Where a failed bootstrap leaves its evidence, how to read the error, common failures and when to re-run |

## Running tenant clusters

| Document | Contents |
|---|---|
| [tenant-clusters/node-lifecycle.md](tenant-clusters/node-lifecycle.md) | What happens to a node through its life |
| [tenant-clusters/kubernetes-api-access.md](tenant-clusters/kubernetes-api-access.md) | How operators, automation and Site Controller services reach and authenticate to tenant Kubernetes APIs |
| [tenant-clusters/oidc-authentication.md](tenant-clusters/oidc-authentication.md) | Turning on OIDC authentication for tenant clusters, site-wide or per cluster |
| [../controller/docs/authenticating-to-the-api.md](../controller/docs/authenticating-to-the-api.md) | Authenticating to the API |
| [../controller/docs/configuring-api-authentication.md](../controller/docs/configuring-api-authentication.md) | Turning API authentication on, including RBAC for `nkectl` |
| [../controller/docs/byo-os.md](../controller/docs/byo-os.md) | Bringing your own OS image |

## Operating a site

| Document | Contents |
|---|---|
| [operations/upgrade.md](operations/upgrade.md) | Upgrading NKE and tenant cluster Kubernetes versions |
| [operations/observability.md](operations/observability.md) | Temporal UI, CWE logs, database health, and CAPNICo diagnostics |
| [operations/faq.md](operations/faq.md) | Common bootstrap and tenant cluster failure modes |
| [nktp/site-telemetry/getting-started.md](nktp/site-telemetry/getting-started.md) | Telemetry on a site controller: what NKTP collects and what the deploy path needs |
| [nktp/site-telemetry/configuration.md](nktp/site-telemetry/configuration.md) | What the site's NKTP values change, connecting a real backend, and overriding values safely |
| [nktp/site-telemetry/monitoring-a-service.md](nktp/site-telemetry/monitoring-a-service.md) | How a service owner gets a service's metrics scraped by NKTP |
| [nktp/site-telemetry/verification.md](nktp/site-telemetry/verification.md) | Checking every telemetry signal end to end after an install |
| [nktp/site-telemetry/troubleshooting.md](nktp/site-telemetry/troubleshooting.md) | Telemetry failures seen on site controllers, and their fixes |

## Running the telemetry pipeline (NKTP)

| Document | Contents |
|---|---|
| [nktp/getting-started.md](nktp/getting-started.md) | NKTP on any cluster: installing the chart, a first look, and what commonly needs changing |
| [nktp/guides/send-telemetry.md](nktp/guides/send-telemetry.md) | For service owners: how a workload's logs, OTLP and Prometheus metrics reach NKTP |
| [nktp/architecture/topology.md](nktp/architecture/topology.md) | The Collectors, how each signal flows to the backend, and how scraping is allocated |
| [nktp/architecture/backpressure.md](nktp/architecture/backpressure.md) | How queues and the memory limiter behave when a Collector is saturated |
| [nktp/architecture/self-telemetry.md](nktp/architecture/self-telemetry.md) | The sidecar that buffers each Collector's own telemetry, and how it samples traces |
| [nktp/guides/size-resources.md](nktp/guides/size-resources.md) | Default requests and limits, and the footprint of a three-node cluster |
| [nktp/guides/configure-cert-manager.md](nktp/guides/configure-cert-manager.md) | What NKTP uses cert-manager for, and installing without it |
| [nktp/guides/secure-gateway-export.md](nktp/guides/secure-gateway-export.md) | TLS and mTLS from the gateways and the watchdog to the backend |
| [nktp/guides/configure-kubelet-metrics.md](nktp/guides/configure-kubelet-metrics.md) | Changing the kubelet scrapes, verifying kubelet certificates, and running beside kube-prometheus-stack |
| [nktp/guides/enable-host-process-metrics.md](nktp/guides/enable-host-process-metrics.md) | Per-process host metrics, the privileges they need, and their cardinality |
| [nktp/components/node-collector.md](nktp/components/node-collector.md) | The node Collector (`daemonset`): OTLP endpoint, pod logs, journal, log classification and monitor credentials |
| [nktp/components/gateways.md](nktp/components/gateways.md) | The logs, metrics and traces gateways (`gateway_*`) and their endpoints |
| [nktp/components/large-target-metrics.md](nktp/components/large-target-metrics.md) | The large-target pool (`large_target_metrics`), sizing large scrapes, and Kubernetes component metrics |
| [nktp/components/cluster-events.md](nktp/components/cluster-events.md) | The cluster event reader (`cluster_events`) and its leader election |
| [nktp/components/watchdog.md](nktp/components/watchdog.md) | The watchdog (`watchdog`), which carries the pipeline's own telemetry |
| [nktp/components/kube-state-metrics.md](nktp/components/kube-state-metrics.md) | kube-state-metrics (`kube-state-metrics`): collection groups, sharding and opt-in Secret metrics |
| [nktp/components/otel-lgtm.md](nktp/components/otel-lgtm.md) | The bundled development backend (`otel-lgtm`), its dashboards and settings |
| [nktp/components/dependencies.md](nktp/components/dependencies.md) | The bundled Operator, CRDs, node-exporter and metrics-server |
| [nktp/design-principles.md](nktp/design-principles.md) | Why the chart renders each Collector configuration exactly as written |
| [nktp/designs/target-allocation.md](nktp/designs/target-allocation.md) | Why every scraping Collector requires a Target Allocator |
| [nktp/designs/kube-state-metrics.md](nktp/designs/kube-state-metrics.md) | The approaches considered for scaling kube-state-metrics collection |

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
| [qualification/overview.md](qualification/overview.md) | Release qualification: what it is, what a run proves, and what is not covered yet |
| [qualification/hardware-lane.md](qualification/hardware-lane.md) | How a candidate is qualified on real hardware, layer by layer |
| [qualification/environments.md](qualification/environments.md) | Declaring the infrastructure a run can execute against |
| [qualification/triage.md](qualification/triage.md) | Telling a site fault from a code defect on a failed hardware run |
| [../controller/integration-test/README.md](../controller/integration-test/README.md) | The integration tests |
| [../controller/nico-sim/docs/e2e-testing.md](../controller/nico-sim/docs/e2e-testing.md) | End-to-end testing for nico-sim's VM backend |

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
