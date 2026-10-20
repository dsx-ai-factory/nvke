# Supplied controller image and chart diagnostics

These producer-neutral interfaces implement basic IV2 checks. They do not build their
subjects, install a supplied chart, exercise tenant workflows, or establish qualification.
Native Linux AMD64 is the initial admitted route. Native runtime acceptance is pending.

Run from `controller/`. `go run ./cmd/artifact-diagnostic --help` documents inputs;
`make test-controller-image-artifact ARTIFACT_ARGS='...'` and
`make test-chart-package-artifact ARTIFACT_ARGS='...'` are convenience entrypoints.
The Go toolchain and dependencies may be acquired; the subject is never rebuilt.

Both modes require `--subject`, `--sha256 sha256:<64 hex>`, a new absolute `--output`
directory and a positive `--timeout` (default 20 minutes, maximum one hour).
Unsupported subjects and missing tools fail before runtime preparation. Run on an
explicitly admitted, dedicated native Linux AMD64 host with network access for dependency
images and Go modules. This interface does not install host tools or alter host networking.

## Controller image

`--kind controller-image` takes a single-native-manifest OCI-export tar, a mandatory
`--version`, and `--cluster iv2-<32 lowercase hex>`. Aggregate/index subjects are refused.
Supply three dependencies by immutable repository@sha256 reference: `--node-image`
(a Kind-compatible AMD64 node), `--postgres-image` (PostgreSQL with pg_isready), and
`--temporal-image` (Temporal CLI with `server start-dev`). Their approved versions and
native execution must be recorded for each acceptance candidate; there are no default images.
Required tools: Go, Docker, Kind and kubectl. The local default Docker context must also report Linux AMD64. Remote/current-context overrides are not inherited.

The fixture owns its uniquely named Kind cluster, its default Docker node container,
private Docker network, node volumes, namespaces, service account binding, ConfigMap, PostgreSQL/Temporal Pods and their storage.
It applies matching-source release CRDs and the CAPI Cluster CRD from the module dependency.
It does not require CAPI provider controllers, SPIRE, tenant machines, or a supplied chart.
The service account has cluster-admin only inside this disposable owned cluster. PostgreSQL
uses trust authentication only on that private fixture network; no real credentials enter it.
The controller uses its normal image command, a complete local configuration adjusted for
fixture services, empty profiles, and disabled optional SPIFFE/NICo/certificate-renewal paths.

The runtime observation records Pod UID, container ID and CRI image identity, relating a
configuration identity through CRI repository digests when necessary. Shared endpoint
assertions require /healthz, /readyz and the exact /api/version expectation. Endpoint success
does not establish controller-manager cache synchronization or tenant workflow coverage.

Each subprocess has a three-minute ceiling inside the execution budget. Cancellation stops
owned command groups. Cleanup has an independent three-minute budget, collects bounded
logs/events, deletes the owned cluster and checks for remaining Kind containers and observed node volumes, and removes the private network. It removes
the private kubeconfig after successful teardown. Failed cleanup retains ownership details
for recovery: `kind delete cluster --name <recorded cluster>`. Caller environments are never
selected implicitly. The parent may use `owned-cluster` for bounded fallback cleanup.

## Chart package

`--kind chart-package` takes an existing `.tgz`. It requires Go, bash, Helm, Mike Farah yq,
and Python 3 with PyYAML. It admits a bounded single-root archive with ordinary files and
no duplicate paths, links, special files or hidden trailing payload. It does not repair a
package, update dependencies, or acquire replacement charts.

Private extraction enables the existing CHART/--chart assertions. Helm loading, strict
linting, rendering, configuration loading/mounts, authentication, providers, SPIRE/node-agent
settings, identity and RBAC remain upstream-owned. The packaging-command assertion and
embedded/source-chart tests are deliberately excluded. Required completion groups cannot
be filtered through caller test flags. The package checksum is rechecked after execution.

## Results and acceptance

`report.json` uses `nvke.dev/artifact-diagnostic/v1`; `execution.log` and renders retain
original diagnostics. Capture, required groups, completion and cleanup are distinct fields.
Missing reports, missing groups and unknown/failed cleanup cannot pass. Image identity,
package-byte integrity and authentication are separate claims. The report is unsigned,
unqualified diagnostic data, not authenticated Run membership or release authority.

Contract tests cover malformed packages/images, platform/identity refusal, endpoint/version
failures, process cancellation and bounded capture. Real supplied-artifact positives,
wrong-version/nonstarting images, contract-breaking packages, native cancellation, and
verified teardown remain acceptance obligations. Do not infer these from unit tests.
Catalog integration and combined chart/image installation remain deferred.

The opt-in `NVKE_TEST_CHART_ARTIFACT=1 go test ./internal/artifactdiagnostic -run TestSuppliedChartContracts`
prepares a package outside the diagnostic, then exercises the supplied package and a valid
package with a required configuration template removed. This can run on a development host;
it does not establish native AMD64 acceptance of the complete diagnostic entrypoint.
