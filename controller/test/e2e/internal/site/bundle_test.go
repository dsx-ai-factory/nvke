// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptLines is the collector without its comments, so a test that forbids a
// word is not tripped by the comment explaining why the word is forbidden.
func scriptLines(script string) []string {
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// The invocation goes through sudo's argument joining, which eats newlines,
// so it has to be one line; and the script's own deadline has to leave room
// in the budget for the fetch and the redaction after it.
func TestBundleCommandIsOneLineAndBounded(t *testing.T) {
	dir, archive := bundlePaths("site-bundle.tar.gz")
	cmd := bundleCommand(9*time.Minute, dir, archive)
	if strings.Contains(cmd, "\n") {
		t.Errorf("the invocation spans lines:\n%s", cmd)
	}
	for _, want := range []string{"timeout -k 30 450 sh ", bundleScriptPath, dir, archive, timelineDir, "SITE_BUNDLE_DEADLINE_SECONDS=450"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the invocation lacks %q:\n%s", want, cmd)
		}
	}
	if short := bundleCommand(30*time.Second, dir, archive); !strings.Contains(short, " 60 sh ") {
		t.Errorf("a budget below the margin should still give the script a minute:\n%s", short)
	}
	if root := asRoot(cmd); !strings.HasPrefix(root, "sudo -n -i sh -c ") {
		t.Errorf("the collector must run as root:\n%s", root)
	}
}

// Two collections never share a directory or an archive on the node: a spec's
// collection that outlives its session must not be overwritten by, or tar the
// half-written files of, the teardown's.
func TestBundlePathsAreDistinctPerDestination(t *testing.T) {
	specDir, specArchive := bundlePaths("/tmp/run/site-bundle.tar.gz")
	finalDir, finalArchive := bundlePaths("/tmp/run/site-bundle-final.tar.gz")
	if specDir != workDir+"/site-bundle" || specArchive != workDir+"/site-bundle.tar.gz" {
		t.Errorf("spec collection paths = %s, %s", specDir, specArchive)
	}
	if finalDir != workDir+"/site-bundle-final" || finalArchive != workDir+"/site-bundle-final.tar.gz" {
		t.Errorf("final collection paths = %s, %s", finalDir, finalArchive)
	}
	for _, p := range []string{specDir, specArchive, finalDir, finalArchive} {
		if p == timelineDir {
			t.Errorf("%s is the sampler's directory", p)
		}
	}
}

// excludesOnly reports whether a script line names paths only to leave them
// out: find's `-prune` group and `! -name` tests. The deploy-state find is
// the one place the collector spells the deny list, to exclude it.
func excludesOnly(line string) bool {
	return strings.Contains(line, "-prune") || strings.Contains(line, "! -name")
}

// The deny list in scrub.go is what the collector must never read. This is
// the static half of that promise; redactBundle's member check is the other.
func TestBundleScriptNeverReadsDeniedPaths(t *testing.T) {
	for _, forbidden := range []string{
		"/etc/kubernetes/pki", "/var/lib/kubelet/pki", "spire.env", "spire-agent.env", "/var/lib/spire-agent",
		"sitespec.yaml", "/values", "get secret", "helm get", "kind get kubeconfig", "/root/.kube", "admin.conf",
		".tsh", "/var/lib/teleport", `cat "$KUBECONFIG_PATH"`,
	} {
		for _, line := range scriptLines(collectSiteBundleScript) {
			if strings.Contains(line, forbidden) && !excludesOnly(line) {
				t.Errorf("the collector reads %q:\n%s", forbidden, line)
			}
		}
	}
	for _, line := range scriptLines(collectSiteBundleScript) {
		if strings.HasPrefix(strings.TrimSpace(line), "copy") && strings.Contains(line, "kubeconfig") {
			t.Errorf("the collector copies a kubeconfig:\n%s", line)
		}
	}
}

// Reads only, on the node and in the cluster: a collector that changed what
// it reports on would be rewriting the evidence.
func TestBundleScriptOnlyReads(t *testing.T) {
	allowedExec := []string{
		"ovn-nbctl --timeout=10 show", "ovn-nbctl --timeout=10 lb-list",
		"ovn-sbctl --timeout=10 show", "ovn-sbctl --timeout=10 list chassis",
	}
	for _, line := range scriptLines(collectSiteBundleScript) {
		if strings.Contains(line, "kubectl") {
			for _, verb := range []string{"delete", "apply", "patch", "edit", "scale", "create", "rollout", "cordon", "drain", "annotate", "label", "taint", "replace", "cp"} {
				if strings.Contains(line, " "+verb+" ") {
					t.Errorf("the collector runs kubectl %s:\n%s", verb, line)
				}
			}
			if strings.Contains(line, " exec ") {
				allowed := false
				for _, a := range allowedExec {
					allowed = allowed || strings.Contains(line, "-- "+a)
				}
				if !allowed {
					t.Errorf("kubectl exec runs something outside the allow list:\n%s", line)
				}
			}
		}
		if strings.Contains(line, "systemctl") {
			for _, verb := range []string{" restart", " stop", " start", " enable", " disable", " kill", " reset-failed", " daemon-reload"} {
				if strings.Contains(line, verb) {
					t.Errorf("the collector changes a unit:\n%s", line)
				}
			}
		}
		if strings.Contains(line, "rm ") && !strings.Contains(line, "$OUT") && !strings.Contains(line, "$out") && !strings.Contains(line, "$TMP") && !strings.Contains(line, "$ARCHIVE") {
			t.Errorf("the collector removes something outside its own directory or archive:\n%s", line)
		}
	}
}

// A dead API server must cost a bounded time per read, not the whole budget.
func TestBundleScriptKubectlIsBounded(t *testing.T) {
	for _, line := range scriptLines(collectSiteBundleScript) {
		if strings.Contains(line, "kubectl --kubeconfig") && !strings.Contains(line, "--request-timeout") {
			t.Errorf("a kubectl call without --request-timeout:\n%s", line)
		}
	}
}

// stubTools puts a fake of every tool the collector calls on PATH. Each prints
// what it was run as; a few misbehave on purpose, which is what the index has
// to record.
func stubTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	plain := "#!/bin/sh\necho \"RAN $(basename \"$0\") $*\"\n"
	stubs := map[string]string{
		"journalctl": plain, "systemctl": plain, "ip": plain, "iptables-save": plain, "ss": plain,
		"ovs-vsctl": plain, "ovs-appctl": plain, "cloud-init": plain, "ctr": plain, "kind": plain,
		"sysctl": plain, "lsmod": plain, "dmesg": plain, "top": plain, "timedatectl": plain, "bridge": plain,
		"apt-cache": plain, "dpkg": plain, "dpkg-query": plain, "nvkectl": plain, "free": plain, "df": plain,
		"lsblk": plain, "uptime": plain,
		// Fails: the index records the status and the rest still runs.
		"nft": "#!/bin/sh\necho 'nft: permission denied' >&2\nexit 7\n",
		// Floods: the cap keeps the first bytes and the index records the SIGPIPE, 141.
		"ovs-ofctl": "#!/bin/sh\nhead -c 10485760 /dev/zero | tr '\\0' x\n",
		// Hangs: its own timeout ends it, and the index says 124.
		"resolvectl": "#!/bin/sh\nsleep 30\n",
		"kubectl": `#!/bin/sh
case "$*" in
  *api-resources*) echo clusters.cluster.x-k8s.io ;;
  *go-template*) echo "kube-system coredns-abc coredns" ;;
  *jsonpath*) echo ovnkube-node-xyz ;;
  *) echo "RAN kubectl $*" ;;
esac
`,
		"helm": `#!/bin/sh
case "$*" in
  *--no-headers*) echo "spire spire-server 1 2026-10-20 deployed spire-0.30.1 1.13.0" ;;
  *) echo "RAN helm $*" ;;
esac
`,
		"crictl": `#!/bin/sh
case "$*" in
  *" ps -a -q "*) echo abc123 ;;
  *inspect*) echo coredns-abc_coredns ;;
  *) echo "RAN crictl $*" ;;
esac
`,
		"nerdctl": `#!/bin/sh
case "$*" in
  *"network ls --format"*) printf 'bridge\nkind\n' ;;
  *"ps -a --format"*) echo nvkectl-bootstrap-control-plane ;;
  *) echo "RAN nerdctl $*" ;;
esac
`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// hostFixture is a node's filesystem as the collector reads it through
// SITE_BUNDLE_HOST_ROOT.
func hostFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	const sixtyFour = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	files := map[string]string{
		"etc/cni/net.d/nerdctl-bridge.conflist":        `{"cniVersion":"1.0.0","name":"bridge","plugins":[{"type":"bridge","bridge":"nerdctl0"}]}`,
		"etc/resolv.conf":                              "nameserver 10.96.0.10\n",
		"etc/os-release":                               "PRETTY_NAME=\"Test OS 1\"\n",
		"etc/kubernetes/manifests/kube-apiserver.yaml": "kind: Pod\n",
		"etc/apt/sources.list.d/ubuntu.sources":        "Types: deb\n",
		"var/log/nke-cni-ovn-postinst.log":             "import ok\n",
		"var/log/cloud-init-output.log":                "done\n",
		"var/lib/nvke/state.json": `{"status":"ready","joinInfo":{"controlPlaneJoinString":"kubeadm join 10.0.0.5:6443 --token abcdef.0123456789abcdef ` +
			`--discovery-token-ca-cert-hash sha256:` + sixtyFour + ` --certificate-key ` + sixtyFour + `","workerJoinString":"kubeadm join 10.0.0.5:6443 --token abcdef.0123456789abcdef"}}`,
		// The deploy state: diagnostics and run state come along with their
		// paths; the rendered deploy path, the deny-listed directories and
		// names stay behind.
		"root/.local/state/nvke/diagnostics/a.txt":                 "deploy state\n",
		"root/.local/state/nvke/run/last-install.json":             "{\"step\":\"nico\"}\n",
		"root/.local/state/nvke/site.kubeconfig":                   "apiVersion: v1\nusers: []\n",
		"root/.local/state/nvke/sitespec.yaml":                     "name: site\n",
		"root/.local/state/nvke/spire.env":                         "SPIRE_JOIN_TOKEN=x\n",
		"root/.local/state/nvke/renders/site/postgres/values.yaml": "auth:\n  password: rendered\n",
		"root/.local/state/nvke/values/stack.yaml":                 "nico:\n  token: rendered\n",
		"root/.local/state/nvke/pki/ca.key":                        "not a key\n",
		"var/lib/nke-e2e/site-timeline/timeline/timeline.log":      "=== 2026-10-20T04:00:00Z\n--- /etc/cni/net.d\n",
		// A registry credential block, which the collector strips on the node.
		"etc/containerd/config.toml": "[plugins.\"io.containerd.grpc.v1.cri\".registry.configs.\"registry.example\".auth]\n" +
			"  username = \"nvke\"\n  password = \"" + fixtureRegistryPassword + "\"\n  auth = \"" + fixtureRegistryBasic + "\"\n",
		"etc/containerd/certs.d/registry.example/hosts.toml": "server = \"https://registry.example\"\n[host.\"https://registry.example\"]\n" +
			"  capabilities = [\"pull\", \"resolve\"]\n  [host.\"https://registry.example\".header]\n    Authorization = [\"Basic " + fixtureRegistryBasic + "\"]\n",
	}
	for p, content := range files {
		writeFile(t, filepath.Join(root, p), content)
	}
	return root
}

// A registry credential for the fixture, built at run time: a literal
// `Basic <base64>` in a test file reads as a leak to the Secrets gate.
var (
	fixtureRegistryPassword = strings.Repeat("p", 12)
	fixtureRegistryBasic    = base64.StdEncoding.EncodeToString([]byte("nvke:" + fixtureRegistryPassword))
)

// readArchive returns the members of a gzipped tar by name.
func readArchive(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a gzip archive: %v", err)
	}
	members := map[string]string{}
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = string(content)
	}
	return members
}

// indexRows parses index.tsv into section -> [exit, bytes, seconds, command].
func indexRows(t *testing.T, index string) map[string][]string {
	t.Helper()
	rows := map[string][]string{}
	for i, line := range strings.Split(strings.TrimSpace(index), "\n") {
		if i == 0 {
			continue
		}
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) < 5 {
			t.Errorf("index row with %d fields: %q", len(fields), line)
			continue
		}
		rows[fields[0]] = fields[1:]
	}
	return rows
}

// The script, run for real against stubs: every section lands in the index
// with its status, the caps hold, a hang ends on its own timeout, the deny
// list is respected, and the manifest says the run completed.
func TestBundleScriptUnderStubbedTools(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("the collector runs its commands under GNU timeout; run this on Linux")
	}
	stubs := stubTools(t)
	root := hostFixture(t)
	work := t.TempDir()
	script := filepath.Join(work, "collect-site-bundle.sh")
	writeFile(t, script, collectSiteBundleScript)
	kubeconfig := filepath.Join(work, "site.kubeconfig")
	writeFile(t, kubeconfig, "apiVersion: v1\n")
	out := filepath.Join(work, "site-bundle")
	archive := filepath.Join(work, "site-bundle.tar.gz")

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", script, out, archive)
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+":"+os.Getenv("PATH"),
		"SITE_BUNDLE_HOST_ROOT="+root,
		"SITE_KUBECONFIG="+kubeconfig,
		"SITE_BUNDLE_DEADLINE_SECONDS=300",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the collector failed: %v\n%s", err, output)
	}

	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("no archive: %v", err)
	}
	members := readArchive(t, data)
	rows := indexRows(t, members["site-bundle/index.tsv"])

	for section, want := range map[string][2]string{
		"node/net/nft-ruleset.txt":                                       {"7", ""},
		"node/ovs/flows-br-int.txt":                                      {"141", "4194304"},
		"node/net/resolvectl.txt":                                        {"124", ""},
		"node/cni/net.d/nerdctl-bridge.conflist":                         {"0", ""},
		"cluster/dns/kube-dns.yaml":                                      {"0", ""},
		"cluster/controller/all.txt":                                     {"0", ""},
		"cluster/capi/clusters.cluster.x-k8s.io.yaml":                    {"0", ""},
		"cluster/pods.previous-logs/kube-system_coredns-abc_coredns.log": {"0", ""},
		"cluster/helm-history/spire-server_spire.txt":                    {"0", ""},
		"cluster/ovn/nbctl-lb-list.txt":                                  {"0", ""},
		"node/runtime/crictl-logs/kube-system_coredns-abc_coredns.log":   {"0", ""},
		"node/seed/nvkectl-bootstrap-control-plane-crictl-ps.txt":        {"0", ""},
		"node/config/nvke-state.json":                                    {"0", ""},
		"cluster/pods.logs/kube-system_coredns-abc_coredns.log":          {"0", ""},
		"timeline/timeline.log":                                          {"0", ""},
		"node/config/containerd-config.toml":                             {"0", ""},
		"node/config/containerd-certs.d.txt":                             {"0", ""},
	} {
		row, ok := rows[section]
		if !ok {
			t.Errorf("no index row for %s; rows:\n%s", section, members["site-bundle/index.tsv"])
			continue
		}
		if row[0] != want[0] {
			t.Errorf("%s exit = %q, want %q", section, row[0], want[0])
		}
		if want[1] != "" && row[1] != want[1] {
			t.Errorf("%s bytes = %q, want %q", section, row[1], want[1])
		}
	}

	manifest := members["site-bundle/manifest.txt"]
	for _, want := range []string{"interrupted: no", "api-reachable: yes", "kubeconfig: present", "os: Test OS 1"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest lacks %q:\n%s", want, manifest)
		}
	}

	state := members["site-bundle/node/config/nvke-state.json"]
	if strings.Contains(state, "abcdef.0123456789abcdef") || !strings.Contains(state, "[stripped]") {
		t.Errorf("the join strings were not stripped on the node:\n%s", state)
	}
	if got := members["site-bundle/node/cni/net.d/nerdctl-bridge.conflist"]; !strings.Contains(got, "nerdctl0") {
		t.Errorf("the CNI conflist was not copied: %q", got)
	}
	for _, want := range []string{"site-bundle/node/logs/deploy-state/diagnostics/a.txt", "site-bundle/node/logs/deploy-state/run/last-install.json"} {
		if _, ok := members[want]; !ok {
			t.Errorf("the deploy path's state was not copied with its path: no %s", want)
		}
	}
	for name := range members {
		if !strings.Contains(name, "/deploy-state/") {
			continue
		}
		for _, denied := range []string{"renders/", "values/", "pki/", "sitespec.yaml", "spire.env", "kubeconfig"} {
			if strings.Contains(name, denied) {
				t.Errorf("a denied deploy-state file was copied: %s", name)
			}
		}
	}
	if got := members["site-bundle/timeline/timeline.log"]; !strings.Contains(got, "/etc/cni/net.d") {
		t.Errorf("the sampler's timeline was not copied in: %q", got)
	}
	for _, member := range []string{"site-bundle/node/config/containerd-config.toml", "site-bundle/node/config/containerd-certs.d.txt"} {
		got := members[member]
		if strings.Contains(got, fixtureRegistryBasic) || strings.Contains(got, fixtureRegistryPassword) || !strings.Contains(got, "[stripped]") {
			t.Errorf("%s still carries the registry credential, or was not stripped:\n%s", member, got)
		}
	}
	for name := range members {
		if strings.Contains(name, "cluster-info/") {
			t.Errorf("the collector still writes a cluster-info dump, which no cap reaches: %s", name)
		}
		if strings.Contains(name, "kubeconfig") {
			t.Errorf("a kubeconfig is in the archive: %s", name)
		}
		for _, d := range deniedMembers() {
			if d.MatchString(name) {
				t.Errorf("a denied member is in the archive: %s", name)
			}
		}
	}
	if _, findings, err := redactBundle(data, []string{"not-in-there-secret"}); err != nil || len(findings) > 0 {
		t.Errorf("the collector's own output does not pass the scrub: %v %v", err, findings)
	}
}

const bundleIndexFixture = "section\texit\tbytes\tseconds\tcommand\n" +
	"node/cni/net.d.listing.txt\t0\t120\t0\tls -la /etc/cni/net.d\n" +
	"node/net/nft-ruleset.txt\t7\t0\t0\tnft list ruleset\n" +
	"cluster/heavy\t-\t0\t0\tskipped: API server not reachable\n"

func TestCollectBundle(t *testing.T) {
	s := newFakeSession()
	dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
	s.fetch[archiveFor(dest)] = tarGz(t, map[string]string{
		"site-bundle/index.tsv":                      bundleIndexFixture,
		"site-bundle/manifest.txt":                   "started: x\ninterrupted: no\n",
		"site-bundle/node/journal/units/kubelet.log": "joined with abcdef.0123456789abcdef as SECRETVALUE1234\n",
	})

	report, err := CollectBundle(t.Context(), s, dest, 9*time.Minute, "SECRETVALUE1234")
	if err != nil {
		t.Fatalf("CollectBundle: %v", err)
	}

	staged, ok := s.puts[bundleScriptPath]
	if !ok || staged.mode != 0o755 || string(staged.data) != collectSiteBundleScript {
		t.Errorf("the collector was not staged executable at %s", bundleScriptPath)
	}
	if run := s.cmd(t, "collect-site-bundle.sh"); !strings.Contains(run, "timeout -k 30 450 sh") {
		t.Errorf("the collector was not run under its deadline: %s", run)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("no bundle written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode = %v, want 0600", info.Mode().Perm())
	}
	written, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	journal := readArchive(t, written)["site-bundle/node/journal/units/kubelet.log"]
	if strings.Contains(journal, "SECRETVALUE1234") || strings.Contains(journal, "abcdef.0123456789abcdef") {
		t.Errorf("the bundle still carries a credential: %q", journal)
	}
	if !strings.Contains(journal, redactedMarker) || !strings.Contains(journal, "REDACTED-TOKEN") {
		t.Errorf("the redactions are not marked: %q", journal)
	}
	if report.Partial || report.Interrupted != "no" || report.Sections != 3 || report.Failed != 1 || report.Skipped != 1 || report.Bytes == 0 {
		t.Errorf("report = %+v", report)
	}
}

// The deadline ends the script with status 124 after its EXIT trap wrote the
// archive, so a failed Run is a partial bundle, not no bundle.
func TestCollectBundleKeepsAPartialArchive(t *testing.T) {
	s := newFakeSession()
	dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
	s.fetch[archiveFor(dest)] = tarGz(t, map[string]string{"site-bundle/index.tsv": bundleIndexFixture, "site-bundle/manifest.txt": "interrupted: deadline\n"})
	s.fail = func(cmd string) (string, error) {
		if strings.Contains(cmd, "collect-site-bundle.sh") {
			return "", errors.New("Process exited with status 124")
		}
		return "", nil
	}
	report, err := CollectBundle(t.Context(), s, dest, 9*time.Minute)
	if err != nil {
		t.Fatalf("CollectBundle: %v", err)
	}
	if !report.Partial || report.Interrupted != "deadline" {
		t.Errorf("report = %+v, want partial and interrupted by the deadline", report)
	}
	if _, statErr := os.Stat(dest); statErr != nil {
		t.Errorf("the partial bundle was not written: %v", statErr)
	}
}

// archiveFor is the node-side archive a collection to dest fetches.
func archiveFor(dest string) string {
	_, archive := bundlePaths(dest)
	return archive
}

// A caller whose context is done gets nothing started on the node: no script
// run, no fetch, no file. The detached context inside collectBundle is for
// the fetch after a run that did start, not a way around the caller's.
func TestCollectBundleStopsOnACanceledContext(t *testing.T) {
	s := newFakeSession()
	dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
	s.fetch[archiveFor(dest)] = tarGz(t, map[string]string{"site-bundle/index.tsv": bundleIndexFixture})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := CollectBundle(ctx, s, dest, 9*time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the cancellation to surface, got %v", err)
	}
	for _, run := range s.runs {
		if strings.Contains(run, "collect-site-bundle.sh") {
			t.Errorf("the collector ran on a canceled context: %s", run)
		}
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("wrote a bundle anyway: %v", statErr)
	}
}

// Status 73 is the script finding another collector on its directory. The
// archive there is that collector's, so nothing is fetched and the caller gets
// ErrBundleBusy rather than a bundle that describes a different moment.
func TestCollectBundleReportsABusyDirectory(t *testing.T) {
	s := newFakeSession()
	dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
	s.fetch[archiveFor(dest)] = tarGz(t, map[string]string{"site-bundle/index.tsv": bundleIndexFixture})
	s.fail = func(cmd string) (string, error) {
		if strings.Contains(cmd, "collect-site-bundle.sh") {
			return "another collector holds /var/lib/nke-e2e/site-bundle", errors.New("Process exited with status 73")
		}
		return "", nil
	}
	_, err := CollectBundle(t.Context(), s, dest, 9*time.Minute)
	if !errors.Is(err, ErrBundleBusy) {
		t.Fatalf("want ErrBundleBusy, got %v", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("wrote the other collector's archive: %v", statErr)
	}
}

func TestCollectBundleWhenTheSessionIsGone(t *testing.T) {
	s := newFakeSession()
	dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
	_, err := CollectBundle(t.Context(), s, dest, 9*time.Minute)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing archive should surface, got %v", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("wrote a bundle anyway: %v", err)
	}
}

// An archive that cannot be shown clean is not written; a note takes its
// place and names what was found, never the value.
func TestCollectBundleFailsClosed(t *testing.T) {
	t.Run("denied member", func(t *testing.T) {
		s := newFakeSession()
		dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
		s.fetch[archiveFor(dest)] = tarGz(t, map[string]string{
			"site-bundle/index.tsv":              bundleIndexFixture,
			"site-bundle/node/config/admin.conf": "apiVersion: v1\n",
		})
		_, err := CollectBundle(t.Context(), s, dest, 9*time.Minute)
		if !errors.Is(err, ErrBundleRedaction) {
			t.Fatalf("want ErrBundleRedaction, got %v", err)
		}
		if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("wrote the bundle anyway: %v", err)
		}
		note, err := os.ReadFile(dest + ".redaction-failed.txt")
		if err != nil {
			t.Fatalf("no note: %v", err)
		}
		if !strings.Contains(string(note), "admin.conf") || !strings.Contains(string(note), "denied member name") {
			t.Errorf("the note does not say what was found:\n%s", note)
		}
	})
	t.Run("not an archive", func(t *testing.T) {
		s := newFakeSession()
		dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
		s.fetch[archiveFor(dest)] = []byte("not a tarball")
		_, err := CollectBundle(t.Context(), s, dest, 9*time.Minute)
		if !errors.Is(err, ErrBundleRedaction) {
			t.Fatalf("want ErrBundleRedaction, got %v", err)
		}
		if _, statErr := os.Stat(dest + ".redaction-failed.txt"); statErr != nil {
			t.Errorf("no note: %v", statErr)
		}
	})
}

func TestCollectBundleRefusesAHugeArchive(t *testing.T) {
	s := newFakeSession()
	dest := filepath.Join(t.TempDir(), "site-bundle.tar.gz")
	s.fetch[archiveFor(dest)] = tarGz(t, map[string]string{"site-bundle/index.tsv": bundleIndexFixture})
	_, err := collectBundle(t.Context(), s, dest, 9*time.Minute, 16, nil)
	if !errors.Is(err, ErrBundleTooLarge) {
		t.Fatalf("want ErrBundleTooLarge, got %v", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("wrote the bundle anyway: %v", err)
	}
}

// A real archive, when one is at hand: SITE_BUNDLE_FIXTURE names a
// site-bundle.tar.gz collected from a node, and this runs it through the same
// redaction the suite applies before upload. Skipped otherwise, so the merge
// gate does not depend on a file nobody checks in.
func TestRedactBundleFixture(t *testing.T) {
	fixture := os.Getenv("SITE_BUNDLE_FIXTURE")
	if fixture == "" {
		t.Skip("SITE_BUNDLE_FIXTURE is not set")
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	out, findings, err := redactBundle(data, []string{"a-value-the-run-never-wrote"})
	if err != nil {
		t.Fatalf("redactBundle: %v", err)
	}
	if len(findings) > 0 {
		t.Fatalf("the fixture does not pass the scrub:\n%s", describeFindings(findings))
	}
	if dest := os.Getenv("SITE_BUNDLE_FIXTURE_OUT"); dest != "" {
		if err := os.WriteFile(dest, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	members := readArchive(t, out)
	var report BundleReport
	fillReport(&report, out)
	t.Logf("%s: %d members, %d bytes in, %d bytes out, sections=%d failed=%d skipped=%d interrupted=%s",
		filepath.Base(fixture), len(members), len(data), len(out), report.Sections, report.Failed, report.Skipped, report.Interrupted)
}
