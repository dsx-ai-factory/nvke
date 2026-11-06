# CIS benchmark scanning in CI: design

> **Status:** implemented in #2156.

## Intent

Every cluster NVKE creates should pass the CIS Kubernetes Benchmark as kube-bench checks it, and CI should show that it still does. Before this work nothing ran a benchmark scan regularly. Earlier hardening came from one-off kube-bench runs (#855 and #975), so a regression was noticed only when someone scanned again or a security review did.

This design adds a kube-bench scan to the e2e lanes that build real clusters: the bootstrap lane, which builds a site cluster, and the CAPD lane, which builds tenant clusters. A scan fails its lane when kube-bench reports a failure or a warning that is not a recorded exception.

## Goals

- kube-bench runs on every run of the bootstrap lane (provisioned suite), against every node of the site cluster, and of the CAPD lane, against every node of one tenant cluster: "CAPD core" on pull requests and "CAPD with etcd encryption" on full runs. The other clusters a full CAPD run builds are not scanned.
- A scan passes only when kube-bench reports no `FAIL` and no `WARN`, other than checks in the exception register. This covers every check kube-bench runs, Level 1 and Level 2, automated and manual.
- The benchmark is kube-bench's `cis-2.0` (CIS Kubernetes Benchmark v2.0): upstream's configuration from a pinned kube-bench commit, run by a pinned kube-bench release. When a release ships `cis-2.0`, the scan uses that release's own copy; when kube-bench adds CIS v2.1.0, the pin moves again. NVKE maintains no benchmark configuration of its own and checks none in.
- CI scans clusters on Kubernetes 1.36, the version NVKE creates by default.
- Exceptions live in one versioned file. Each records the check, where it applies, why, who approved it, and when to review it.
- Every finding is fixed in NVKE or recorded in the register.
- Every scan's full results are kept as a run artifact, whether the scan passed or failed.

## Non-goals

- Blocking merges. The lanes stay advisory: they run on pull requests that touch their paths and twice a day on `main`, and they are not part of the merge gate.
- Scanning running clusters on a schedule, exporting results through NKTP, and alerting. That is a follow-up.
- Notifying anyone when a scheduled run fails. `main-health-check.yml` deliberately watches only merge-gate members; a failed scan shows as a red lane on the pull request that caused it and on the scheduled runs.
- A benchmark configuration maintained or vendored in this repository. Checks kube-bench's `cis-2.0` gets wrong or cannot see are recorded as exceptions with the evidence, until a kube-bench release does better.
- Enabling the EventRateLimit admission plugin (1.2.9), and changing the CI node images to match the production image's etcd data directory owner (1.1.12). See **Decisions**.
- Enforcing the benchmark's workload policies (section 5) on clusters customers run. What runs in a workload cluster is the customer's to decide; those checks are recorded as exceptions with that reason.
- Hardening NVKE's own components against section 5: Pod Security labels, a NetworkPolicy per namespace, default service account token mounts, and RBAC narrower than `cluster-admin`, across the management-plane charts and the RBAC NVKE installs in tenant clusters. Findings on NVKE's own objects are recorded as exceptions, each naming the component and why, and the hardening is a follow-up.
- Existing clusters. A fix the scans led to applies to clusters created after it ships.
- Binding kube-proxy's metrics to loopback (4.3.1). Neither lane runs kube-proxy, so the scan cannot see the setting, and #1316 proposed it and was closed.
- The security sign-off process itself. The register records who approved each exception; obtaining that approval happens outside this work.

## Anti-goals

The work has failed, even if CI is green, if:

- A check passes because it was excepted without a recorded reason, or an exception claims NVKE meets a check without pointing at the evidence.
- A setting is changed to satisfy a check at the cost of breaking clusters. `--anonymous-auth=false` broke every bring-up suite in #855; #975 met the same intent differently.
- CIS Benchmark text is copied into the repository. The benchmark is under CIS's own terms of use, and this repository is public. Exception reasons are written in our own words.
- An e2e flake starts blocking merges.

## Background

### kube-bench

- The latest release is v0.16.0 (5 August 2026). Its newest benchmark is `cis-1.12`, which it maps to Kubernetes 1.32 to 1.34; it maps nothing to 1.35 or 1.36. Docker Hub's `latest` is the same image.
- `cis-2.0` was added on kube-bench's main branch on 18 August 2026, in commit `9f133cb7509ce1dbedfc860e94474588000e25ac` (#2126), after v0.16.0. No release or published image has it yet. The commit adds six files under `cfg/cis-2.0/` and maps Kubernetes 1.34 and 1.35 to `cis-2.0` in `cfg/config.yaml`; it changes no Go code.
- The v0.16.0 binary runs `cis-2.0` when it is given those seven files with `--config-dir`: all five sections load and report as `cis-2.0`. Together the files are about 110 KB.
- `--benchmark cis-2.0` selects the benchmark explicitly, which a 1.36 cluster needs. `--skip` exists but is not used here; see **Decisions**.
- kube-bench is Apache-2.0, like NVKE.

### The benchmark

CIS v2.1.0 (30 September 2026) differs from v2.0.1 in four recommendations. They apply once kube-bench supports v2.1.0:

| Check | Change | Level |
|---|---|---|
| 1.2.1 | Rewritten: anonymous requests to the API server are restricted. Anonymous access may stay enabled if the authentication configuration limits it to the health endpoints. | 1 |
| 4.1.11 | New, automated: the kubelet's drop-in configuration directory and files have restrictive permissions. | 1 |
| 5.1.14 | New, manual: access to the kubelet API uses fine-grained subresources rather than `nodes/proxy`. | 2 |
| 5.2.13 | New, manual: minimise admission of containers with unmasked procmounts. | 1 |

v2.0.0 and v2.0.1 have identical recommendations; v2.0.1 only widened the supported versions.

### The clusters CI builds

- **Bootstrap lane:** a site cluster on Kubernetes 1.36.4 with OVN-Kubernetes, made of three control-plane nodes and no workers. The nodes are privileged containers running systemd (`cli/e2e/bootstrap/substrate`), and the control-plane taint is removed. `run.sh` waits for every node to be Ready in stage 4 and checks the taint in stage 4c. The node image is built on the CAPD node image, and `/var/lib/etcd` is a tmpfs mounted when the container starts (`substrate.go`).
- **CAPD lane:** tenant clusters on Kubernetes 1.36.3 with OVN-Kubernetes. On pull requests the "CAPD core" entry runs, with one control-plane node and one worker. Scheduled and full runs run every CAPD entry, including "CAPD with etcd encryption", with three control-plane nodes (`controller/test/e2e/tenant_test.go`).
- **Local entrypoint lane:** runs the CAPD rows through `controller/hack/local-backend-e2e.sh`.
- All moved to 1.36 in #2132. A CAPD node's kubelet and kubeadm come from its base image, `kindest/node:v1.36.4`, which is also the base of the bootstrap lane's nodes. Tenant control planes run `defaultKubernetesVersion`, 1.36.3; a site cluster runs its node's own version, 1.36.4.
- The e2e suite can run against any cluster it is pointed at: the validation image runs its site checks that way. `TestE2E` needs no config file, and specs are selected by label.

### Configuration NVKE renders for the benchmark

`controller/internal/kubeadmconfig/render.go` cites CIS 1.2.18, 1.2.29, 1.2.30, 1.3.1, 1.3.7, 1.4.2, 4.2.12, 4.2.13 and 4.2.14, and `init.go` cites 1.2.1. In particular:

- **1.2.1:** the API server's authentication configuration allows anonymous requests only to the health and version endpoints and the public cluster-info ConfigMap kubeadm joins read (`anonymousProbeAuthConfig`, #975). kube-bench's `cis-2.0` check reads only the `--anonymous-auth` flag, so it cannot see this and reports `WARN`.
- **4.2.12:** the kubelet's `tlsCipherSuites` is set on every node (`kubeletTLSCipherSuites`), with the ChaCha20 suites under their IANA names, ending in `_SHA256`. The `cis-2.0` check's allowlist spells those two suites only without the suffix, so it reports `WARN`; its API server check (1.2.29) accepts both spellings, and the API server's identical list passes.
- **kube-proxy:** not deployed on OVN-Kubernetes clusters (`SkipKubeProxy` in `temporal/activities/kubeadm_init.go`), which is the API default. With no kube-proxy process, kube-bench's 4.3.1 audit command exits non-zero and kube-bench reports `FAIL`. `metricsBindAddress` is still rendered as `0.0.0.0:10249`; binding it to loopback was proposed in #1316 and closed.
- **1.1.11 and 1.1.12:** kubeadm creates `/var/lib/etcd` with mode 0700. The production OS image also creates it as `etcd:etcd` (`os-image/packer/scripts/provision.sh`, #944); the CI node images, built on `kindest/node`, leave it to kubeadm, which makes it `root:root`. etcd runs as root in a kubeadm static pod either way, so the owner does not change what it can access. kube-bench fails production nodes too: its audit runs `stat -c %U:%G` inside the kube-bench image, which has no `etcd` user or group, so the production owner reads as `UNKNOWN:UNKNOWN`.
- **Admission plugins:** `defaultEnabledAdmissionPlugins` replaces kubeadm's list, and EventRateLimit is not in it. There is no `AdmissionConfiguration` file.
- **Encryption at rest (1.2.27, 1.2.28):** a per-cluster choice. `etcdEncrypted` is required on every create request and can be turned on later, but not off. When it is on, NVKE configures the `secretbox` provider, which both checks accept.

## Decisions

These were decided with the person requesting the work:

- Scan everything kube-bench checks, Level 1 and Level 2, rather than Level 1 only.
- Use kube-bench's upstream `cis-2.0`. No release ships it yet, so the scan runs the v0.16.0 binary with upstream's `cis-2.0` files from a pinned commit, fetched when the scan runs and never checked in. Move to a release's own copy once one ships, and to CIS v2.1.0 once kube-bench supports it. NVKE maintains no benchmark configuration.
- The scan does not block merges.
- Scheduled in-cluster scanning and NKTP alerting are a separate, later piece of work.
- Section 5 findings on NVKE's own components are recorded as exceptions with specific reasons; hardening those components is a follow-up.
- CI scans Kubernetes 1.36 clusters, the version NVKE creates by default. Every lane built on the CAPD node image moved to 1.36 together, in its own pull request before the scan was added: #2132, which also implements part of #2041 by moving CI and the development defaults off 1.34.
- kube-proxy's metrics address is left as it is.
- EventRateLimit (1.2.9) is not enabled. kube-bench reports the check as `WARN`, not `FAIL`. Client libraries already filter and aggregate repeated events, API Priority and Fairness bounds what any one client can take of the API server, and events expire after an hour. The plugin's configuration API is still `v1alpha1`, and limits loose enough not to throttle customer workloads would rarely act. The check is recorded in the register with those reasons.
- The bootstrap lane runs the scan through the e2e suite rather than a separate command.
- The CI node images are not changed for 1.1.12. Owning `/var/lib/etcd` as `etcd` changes what a node reports, not what etcd can do, and the scan never sees the production image, so making the CI images match would only turn a CI result green. The check is recorded in the register, pointing at `provision.sh`.
- The CAPD lane scans encrypted clusters: "CAPD core" now creates one, and full runs scan "CAPD with etcd encryption". The scan then checks the encryption configuration NVKE renders when a customer chooses it, which a register entry for an unencrypted cluster could not tell apart from a regression.
- The bootstrap substrate mounts its `/var/lib/etcd` tmpfs with mode 0700, the mode kubeadm gives the directory it creates, rather than recording 1.1.11 as an exception for tmpfs's default 1777.
- NVKE sets the controller manager's `--terminated-pod-gc-threshold` to 12500, Kubernetes' own default, so 1.3.1 passes without changing how many finished pods a cluster keeps.
- The fixes, the register and this document are in the same pull request as the scan.

These are the author's calls, open to change:

- **`cis-2.0` for every version, selected explicitly.** kube-bench maps nothing to 1.36, so CI passes `--benchmark cis-2.0` everywhere.
- **Apply the register to results, never as `--skip`.** kube-bench reports a skipped check as `INFO` without running it, so a skipped check could never be seen to pass again. The scan runs every check, and the runner applies the register when it evaluates the results.
- **Fail on stale register entries.** An exception for a check that now passes, or for an ID kube-bench does not have, fails the scan. A register that is never pruned stops meaning anything, and this is what retires the entries that exist only because of `cis-2.0`'s limits once kube-bench is upgraded.
- **Report, don't fail, on approval and review dates.** An entry with no `approver` still counts, and the summary lists it as awaiting approval; an entry past its `reviewBy` date is listed as due for review. Failing on either would keep the lanes red until an approval outside this work, or turn scheduled runs red on a calendar date with no change behind it.
- **Record evidence where `cis-2.0` cannot see compliance.** For a check NVKE meets by a means `cis-2.0` does not inspect, such as 1.2.1 and 4.2.12, the exception names the setting and where NVKE renders it, rather than only saying it is compliant.
- **One implementation of the scan, in `controller/test/e2e`.** The bootstrap lane's Go code is in the `cli` module and the CAPD lane's in `controller/test/e2e`; the only module both require is `controller/api`, and every change there publishes a new API module tag. The scan lives in `controller/test/e2e/internal/kubebench` with the register embedded in it. The CAPD lane calls it from a tenant validation; the bootstrap lane runs the e2e suite against the site with a label filter that selects one site spec. This avoids a copy of the scan in `cli`, a new module, and a new `main` package.
- **Fetch the benchmark files on the CI host, check them, and deliver them through a ConfigMap.** Checking them in would put CIS text in the repository (see **Anti-goals**) and vendor about 110 KB of third-party YAML that is deleted once a release ships `cis-2.0`. Fetching on the CI host rather than in the cluster means tenant clusters need no route to GitHub. The commit pin and a SHA-256 per file mean the content cannot change under the scan; a mismatch fails the scan before anything is created.
- **Run the policies checks first and alone.** Section 5 lists the cluster's pods, so the scan's own pods would otherwise be reported against the cluster: the node pods share the host's PID namespace (5.2.3), and pods running as the namespace's default service account mount a token (5.1.6). The policies pod shares nothing with the host but the admin kubeconfig, and runs as a service account of its own that mounts no token. The node pods start once it has finished.
- **A policies report with no audit rows is an error.** If kubectl in the policies pod cannot read the cluster, every automated section 5 check warns, which looks like ordinary findings and which the tenant register would except.
- **Retry what is transient.** A download that fails in transit, with a server error or with rate limiting is retried, and polling the pods keeps going through a failed read until the scan's timeout. A missing file is not retried.
- **Pods, not Jobs.** Each scan pod is pinned with `nodeName`, collected from its log and then deleted. A Job adds nothing for a single run, and the tenant client has no `batch/v1` scheme.
- **The kube-bench image and the benchmark files are pinned in the scan package,** the image by tag and digest and the files by commit and SHA-256, rather than in `make/`: the pins there are tool binaries, and the Go suites read none of them.

## Design

### Layout

```
controller/test/e2e/
├── cis_benchmark_test.go  # the tenant validation (CAPD lane) and the site spec, labelled cis-site (bootstrap lane)
└── internal/kubebench/
    ├── exceptions.yaml    # the exception register, embedded in the package
    ├── README.md          # how to scan a cluster by hand, add or retire an exception, and upgrade kube-bench
    └── *.go               # the benchmark files, the scan pods, report parsing, the register, evaluation, artifacts
```

The package pins the kube-bench image, `docker.io/aquasec/kube-bench:v0.16.0` by digest, which is published for amd64 and arm64 like the lanes. Beside it are the benchmark name, `cis-2.0`, and the benchmark files: kube-bench commit `9f133cb7509ce1dbedfc860e94474588000e25ac` and the SHA-256 of each of `cfg/config.yaml` and `cfg/cis-2.0/{config,master,controlplane,etcd,node,policies}.yaml`. Moving to a kube-bench release that ships `cis-2.0` changes the image pin and deletes the file pins, the fetch and the ConfigMap; moving to CIS v2.1.0 changes the image and the benchmark name.

### The exception register

`controller/test/e2e/internal/kubebench/exceptions.yaml`:

```yaml
exceptions:
  - id: "1.2.11"
    title: Pulling every image on every start
    scope:            # where the exception applies; an omitted list means all
      roles: [control-plane]
    reason: >-
      Pulling every image on every container start defeats the images
      pre-loaded on nodes and air-gapped installs, and multiplies registry load
      for large GPU images.
    approver: ""      # filled when the exception is approved
    reviewBy: "2027-05-04"
```

`clusters` takes `site` and `tenant`; `roles` takes `control-plane` and `worker`. Two entries for the same check may not cover the same cluster and role. IDs and dates are quoted; an unquoted ID that YAML reads as a number is a load error, because `5.10` would otherwise become `5.1`.

The register was finished from the first CI scans on 1.36, which were identical on amd64 and arm64:

- **Checks NVKE meets by a setting `cis-2.0` does not inspect,** with the evidence: 1.2.1 (anonymous access), 4.2.12 (kubelet cipher suites), 4.2.9 (kubelet serving certificates from `serverTLSBootstrap`), 3.2.2 (the audit policy).
- **Checks with nothing to inspect on these clusters:** 1.1.9 and 1.1.10 (kube-bench finds CNI configuration through a kubelet flag current kubelets no longer have), and 4.1.3, 4.1.4 and 4.3.1 (no kube-proxy on OVN-Kubernetes).
- **Settings NVKE keeps, with the reason:** 1.2.9 (EventRateLimit), 1.2.11 (`AlwaysPullImages`), 1.2.20 (the API server's default request timeout), 1.1.12 (the CI images' etcd directory owner), 3.1.1 to 3.1.3 (the client-certificate administrator kubeconfig), and 4.2.7 on the site (kubeadm passes `--hostname-override` because NVKE registers nodes under their own names).
- **Section 5 on tenant clusters:** workload policy on clusters customers own.
- **Section 5 on the site cluster:** one entry per check, naming what the site's audits found: kubeadm's `kubeadm:cluster-admins` and the `nvke:breakglass` binding to `cluster-admin`, Kubernetes' own wildcard roles, the control-plane static pods and kube-vip, CoreDNS, and OVN-Kubernetes.

### Fixes

- **The controller manager's `--terminated-pod-gc-threshold` (1.3.1):** set to 12500, Kubernetes' own default, on every cluster NVKE creates (`defaultTerminatedPodGCThreshold`).
- **Encryption at rest on scanned tenants (1.2.27, 1.2.28):** "CAPD core" creates an encrypted cluster, and full runs scan "CAPD with etcd encryption".
- **The bootstrap substrate's etcd tmpfs (1.1.11):** mounted with mode 0700 (`substrate.go`).

### Moving CI to Kubernetes 1.36

Done in #2132, before the scan. The CAPD node image's base moved to `kindest/node:v1.36.4`, tenant defaults to 1.36.3, and site clusters and the control-plane roll target to 1.36.4. The other 1.34.2 defaults moved to 1.36.3, the upgrade test now goes from 1.35.3 to 1.36.3, and the e2e kubectl image is `alpine/k8s` 1.36.4.

### Running the scan

Before it creates anything, the scan downloads the seven benchmark files from `raw.githubusercontent.com/aquasecurity/kube-bench/<commit>/cfg/`, checks each against its SHA-256, and fails on a mismatch. It puts them in one ConfigMap in `default`, whose `items` map them back to the layout kube-bench expects (`config.yaml`, `cis-2.0/master.yaml` and so on).

Each pod runs `kube-bench --config-dir <mount> --benchmark cis-2.0 run --json` with the ConfigMap mounted, tolerates every taint and is pinned with `nodeName`:

1. **The policies pod** runs alone, first, on the first control-plane node by name, with the `policies` target and `KUBECONFIG` set to the host's `/etc/kubernetes/admin.conf`, which is all it mounts from the host. It runs as a per-scan service account that mounts no token, and does not share the host's PID namespace.
2. **One node pod per node** then runs, sharing the host's PID namespace and mounting, read-only, the host paths kube-bench's own job manifest mounts: the `master`, `controlplane`, `etcd` and `node` targets on control-plane nodes, and `node` on workers.

kube-bench logs to the same stream, so a report is the log line that begins `{"Controls":`. The policies report counts as the first control-plane node's. On every path, including a timed-out or canceled scan, the scan deletes its pods, the ConfigMap and the service account, once each and within a minute; a deletion that fails is logged as a warning, and anything it leaves behind carries the label `app.kubernetes.io/name=kube-bench`. It evaluates the reports against the register:

- A `FAIL` or `WARN` is excepted when an entry has its ID and covers the node's cluster and role, and is a violation otherwise. `PASS` and `INFO` need nothing.
- An entry is judged over the nodes it covers. It is stale when none of them reported its ID, or when they did and none reported `FAIL` or `WARN`. An entry that covers no node of this cluster, such as a site-only entry on a tenant, is not judged.
- The scan passes when it has no violations and no stale entries. Entries with no approver and entries past `reviewBy` are listed in the summary and do not fail it.

It writes `results.json`, with every node's report and the evaluation, and `summary.txt`, which lists each violation with its ID, node and kube-bench's output, then the stale entries, then the entries awaiting approval or review.

#### Site cluster (bootstrap lane)

- `run.sh` runs the scan as stage 4d, after stage 4c, while the cluster is up and every node is Ready: `NVKE_CIS_SITE_SCAN=true KUBECONFIG=<site kubeconfig> go test -C controller/test/e2e -run '^TestE2E$' . -args -ginkgo.label-filter=cis-site -ginkgo.fail-on-empty -e2e.artifacts-dir=<diagnostics>`. The `cis-site` spec scans the cluster in `KUBECONFIG` as cluster kind `site`, and skips unless `NVKE_CIS_SITE_SCAN` is `true`; its files land in `cli/cluster-diagnostics/kube-bench/`.
- `cis-site` stays out of the validation image's selection, which a unit test checks. Its node pods are privileged and share the host's PID namespace, which the image's minimal permissions rule out.
- The scan writes `summary.txt` only once it has judged the cluster, so stage 4d tells a scan that never reached a verdict apart from one that found violations. Either is logged, does not stop stage 5, and fails the script at its very end, after the suite's own teardown.
- The diagnostics directory is already searched for credentials on every run and deleted when the search finds one. `nvkectl-bootstrap-e2e.yml` uploads the scan's directory with `if: always()` on the provisioned suite, since diagnostics are uploaded only on failure.

#### Tenant clusters (CAPD and local entrypoint lanes)

- A `cisBenchmark` validation flag on "CAPD core" (pull requests) and "CAPD with etcd encryption" (scheduled and full runs). Its spec comes right after node readiness in `Describe("after cluster creation")`, so it scans the cluster as created, before other specs scale, reboot or replace nodes. No extra cluster is built. A tenant scan times out after 5 minutes, leaving the CAPD core leg time for its diagnostics.
- It uses the tenant's admin REST config (`test.tenantClients.AdminRESTConfig`).
- It writes to a suite flag, `-e2e.artifacts-dir`, which `e2e_run_ginkgo` sets to `/state/artifacts`. `backend-e2e.sh` copies that directory out with the other artifacts, before the redaction pass; `local-backend-e2e.sh` copies it beside `junit.xml`.
- A runtime `Skip()` fails the run unless listed in `E2E_EXPECTED_RUNTIME_SKIPS`, so the tenant scan never skips at runtime.
- A local run of the CAPD rows scans too, and needs GitHub and Docker Hub; `&& !cis-benchmark` in `LOCAL_NKE_E2E_LABEL_FILTER` leaves it out.

### Testing

- Unit tests for fetching the benchmark files (a checksum mismatch refuses the scan, a server error is retried, a missing file is not), the scan pods, the order of the policies and node pods, parsing kube-bench's log, loading and validating the register, and evaluating reports (`FAIL`, unlisted `WARN`, stale entries, entries awaiting approval or past review), using synthetic kube-bench reports.
- A live run against a kind cluster on Kubernetes 1.36.4 before CI, which showed the scan's own pods counted as compliant in 5.1.6 and 5.2.3.
- Every lane that scans, green on both architectures with the fixes and the register in place.
- A temporary commit that stopped rendering the API server's `--service-account-extend-token-expiration=false`, with the kubeadm goldens regenerated so unit tests and lint stayed green, turned the scan red on exactly that check, 1.2.30, and nothing else: on CAPD core and on each site control-plane node, on amd64 and arm64.
