# kube-bench scan

This package scans a cluster with [kube-bench](https://github.com/aquasecurity/kube-bench) and judges the result against NVKE's exception register, [`exceptions.yaml`](exceptions.yaml).

It runs in three CI lanes:

- **The CAPD lane** scans the "CAPD core" tenant cluster on pull requests and "CAPD with etcd encryption" on full runs, right after it is created (`validateCISBenchmarkPostCreate` in `cis_benchmark_test.go`, which the `cis-benchmark` label selects). Both are encrypted at rest, so the scan checks the encryption configuration NVKE renders.
- **The local entrypoint lane** runs the same CAPD rows through `controller/hack/local-backend-e2e.sh`, so it scans them too. So does a local run of those rows, such as `make test-e2e-core-capd`. That needs access to GitHub and Docker Hub; to leave the scan out, add `&& !cis-benchmark` to `LOCAL_NKE_E2E_LABEL_FILTER`.
- **The bootstrap lane** scans the site cluster in stage 4d of `cli/e2e/bootstrap/run.sh`, through the `cis-site` spec.

None of these lanes is part of the merge gate.

## What a scan does

1. Downloads kube-bench's `cis-2.0` benchmark files from the pinned upstream commit (`BenchmarkCommit`) and checks each against its SHA-256 (`BenchmarkFiles`). A mismatch stops the scan before it creates anything. No kube-bench release ships `cis-2.0` yet, and the files are never checked into this repository.
2. Puts the files in a ConfigMap in `default` and runs the pinned image (`Image`) with `kube-bench --config-dir … --benchmark cis-2.0 run --json`:
   - First, alone, the `policies` target, on the first control-plane node by name, through that node's admin kubeconfig. Section 5 lists the cluster's pods, so this pod carries none of what it checks for: it does not share the host's PID namespace, and runs as a service account of its own that mounts no token. If its audits return no rows, kubectl could not read the cluster, and the scan stops rather than report every section 5 check as a finding.
   - Then one pod per node, sharing the host's PID namespace: control-plane nodes run the `master`, `controlplane`, `etcd` and `node` targets, and workers run `node`.
3. Reads each pod's report from its log, deletes the pods and the ConfigMap, and evaluates the reports. Section 5 counts as the first control-plane node's:
   - a `FAIL` or `WARN` is excepted when a register entry has its ID and covers the node's cluster (`site` or `tenant`) and role (`control-plane` or `worker`), and is a violation otherwise;
   - an entry is stale when kube-bench did not report its ID on the nodes it covers, or reported it and it passed everywhere;
   - the scan passes with no violations and no stale entries. Entries with no approver, or past their `reviewBy` date, are listed and do not fail it.
4. Writes `results.json` (every report and the evaluation) and `summary.txt`.

## Scanning a cluster by hand

From the repository root, with a kubeconfig that can create privileged pods:

```bash
NVKE_CIS_SITE_SCAN=true KUBECONFIG=<kubeconfig> \
  go test -C controller/test/e2e -run '^TestE2E$' -timeout 30m . \
  -args -ginkgo.label-filter=cis-site -e2e.artifacts-dir=/tmp/kube-bench
```

The spec judges the cluster as a site cluster. Read `/tmp/kube-bench/kube-bench/summary.txt`.

## Adding an exception

Add an entry to `exceptions.yaml`:

```yaml
  - id: "1.2.11"
    title: Pulling every image on every start
    scope:
      roles: [control-plane]
    reason: >-
      Why NVKE does not meet the check, or how it meets it by means kube-bench
      does not inspect, with the file or setting that shows it.
    approver: ""
    reviewBy: "2027-05-04"
```

- Write the title and reason in your own words. Do not copy benchmark text.
- Where NVKE meets the check another way, name where: a file, a function, a pull request.
- Scope it no wider than the findings it covers. Two entries for one check may not overlap.
- Quote `id` and `reviewBy`.
- Leave `approver` empty until the exception is signed off; the summary lists it as awaiting approval.

`make test` in `controller/test/e2e` loads the register (`TestDefaultRegister`), so a malformed entry fails there before it reaches a lane.

## Retiring an entry

A stale entry fails the scan, and the summary says why. Delete it: the check now passes, or kube-bench no longer has it.

## Upgrading kube-bench

- When a kube-bench release ships `cis-2.0`: change `Image`, delete `BenchmarkCommit`, `BenchmarkURL`, `BenchmarkFiles`, the fetch and the ConfigMap, and drop `--config-dir` from `NewPod`.
- For CIS v2.1.0: change `Image` and `Benchmark`. v2.1.0 rewrites 1.2.1 and adds 4.1.11, 5.1.14 and 5.2.13, so expect register changes for those.

Then rescan both lanes and update the register from their summaries.
