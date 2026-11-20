# kube-bench scan

This package scans a cluster with [kube-bench](https://github.com/aquasecurity/kube-bench) and judges the result against NVKE's exception register, [`exceptions.yaml`](exceptions.yaml).

It runs in four CI lanes:

- **The CAPD lane** scans the "CAPD core" tenant cluster on pull requests and "CAPD with etcd encryption" on full runs, right after it is created (`validateCISBenchmark` in `cis_benchmark_test.go`, whose specs the `cis-benchmark` label selects). Both are encrypted at rest, so the scan checks the encryption configuration NVKE renders.
- **The local entrypoint lane** runs the same CAPD rows through `controller/hack/local-backend-e2e.sh`, so it scans them too. So does a local run of those rows, such as `make test-e2e-core-capd`. That needs access to Docker Hub; to leave the scan out, add `&& !cis-benchmark` to `LOCAL_NKE_E2E_LABEL_FILTER`.
- **The bootstrap lane** scans the site cluster in stage 4d of `cli/e2e/bootstrap/run.sh`, through the `cis-site` spec.
- **The NICo VMs lane** scans two encrypted tenant clusters whose nodes boot the NVKE FDE node image: "nico-vms" after creation and again while its worker pool is scaled up, and "nico-vms k8s upgrade" after creation and again once the upgrade has replaced its control plane. A finding that appears only after the scale or the upgrade is a regression to fix, not an entry to add.

None of these lanes is part of the merge gate.

## What a scan does

1. Runs the pinned image (`Image`) in `default` with `kube-bench --benchmark cis-1.12 run --json`:
   - First, alone, the `policies` target, on the first control-plane node by name, through that node's admin kubeconfig. Section 5 lists the cluster's pods, so this pod carries none of what it checks for: it does not share the host's PID namespace, and runs as a service account of its own that mounts no token. If its audits return no rows, kubectl could not read the cluster, and the scan stops rather than report every section 5 check as a finding.
   - Then one pod per node, sharing the host's PID namespace and mounting the host's `/etc/passwd` and `/etc/group`, so kube-bench names file owners as the host does: control-plane nodes run the `master`, `controlplane`, `etcd` and `node` targets, and workers run `node`.
2. Reads each pod's report from its log, deletes the pods and the service account, and evaluates the reports. Section 5 counts as the first control-plane node's:
   - a `FAIL` or `WARN` is excepted when a register entry has its ID, and is a violation otherwise;
   - an entry is stale when kube-bench reported no check with its ID;
   - the scan passes with no violations and no stale entries. An entry whose check passed on every node is listed as unneeded and does not fail it, since the other kind of cluster may still need it.
3. Writes `results.json` (every report and the evaluation) and `summary.txt`.

## The benchmark

`Benchmark` is `cis-1.12`, the newest CIS Kubernetes benchmark the pinned kube-bench release ships. The scan names the benchmark because the release maps no Kubernetes version after 1.34.

## Scanning a cluster by hand

From the repository root, with a kubeconfig that can create privileged pods:

```bash
NVKE_CIS_SITE_SCAN=true KUBECONFIG=<kubeconfig> \
  go test -C controller/test/e2e -run '^TestE2E$' -timeout 30m . \
  -args -ginkgo.label-filter=cis-site -e2e.artifacts-dir=/tmp/kube-bench
```

The results name the cluster a site cluster; the register is the same for every cluster, so any cluster is judged the same way. Read `/tmp/kube-bench/kube-bench/summary.txt`.

A deployed cluster reports findings the CI clusters do not, and the spec counts them as violations:

- 1.2.27 and 1.2.28, when the cluster was created without `etcdEncrypted`.
- Section 5 checks every workload on the cluster, not only NVKE's. On a site that runs NKTP, its node exporter shares the host's PID namespace and fails 5.2.3; the bootstrap lane does not install NKTP, so the register has no entry for it.
- Settings NVKE has started rendering since the cluster's control plane was configured.

## Adding an exception

Add an entry to `exceptions.yaml`:

```yaml
  - id: "1.2.11"
    title: Pulling every image on every start
    reason: >-
      Why NVKE does not meet the check, or how it meets it by means kube-bench
      does not inspect, with the file or setting that shows it.
```

- Write the title and reason in your own words. Do not copy benchmark text.
- Where NVKE meets the check another way, name where: a file, a function, a pull request.
- One entry per check. It applies on every node of every cluster, so where the reason differs between the site and tenant clusters, give both.
- Quote `id`.

`make test` in `controller/test/e2e` loads the register (`TestDefaultRegister`), so a malformed entry fails there before it reaches a lane.

## Retiring an entry

A stale entry fails the scan: kube-bench has no check with its ID, because the ID is wrong or a kube-bench upgrade dropped or renumbered the check. Correct or delete it.

The summary lists an entry as unneeded when its check passed on every node of that cluster. Delete it once the site and tenant scans both list it as unneeded.

## Upgrading kube-bench

Change `Image` and `Benchmark` together: `Benchmark` is the newest CIS Kubernetes benchmark the release ships. A release with `cis-2.1` rewrites 1.2.1 and adds 4.1.11, 5.1.14 and 5.2.13, so expect register changes for those.

Then rescan both lanes and update the register from their summaries.
