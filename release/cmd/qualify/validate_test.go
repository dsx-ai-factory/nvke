// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"sigs.k8s.io/yaml"
)

func writeKeptRun(t *testing.T, record string) string {
	t.Helper()
	run := t.TempDir()
	keys := filepath.Join(run, "node-keys")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if record != "" {
		if err := os.WriteFile(filepath.Join(keys, "site-a.json"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	steps := "SITE_NVKE_CONTROLLER_VERSION=b93b9fd4aa0a\nKUBERNETES_VERSION=1.36.3\n"
	if err := os.WriteFile(filepath.Join(run, "steps.env"), []byte(steps), 0o600); err != nil {
		t.Fatal(err)
	}
	return run
}

func newHostKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey(), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func TestKeptSite(t *testing.T) {
	_, hostKey := newHostKey(t)
	run := writeKeptRun(t, `{"name":"site-a","address":"10.0.0.7","user":"ubuntu","jumpHost":"jump","jumpLogin":"root","key":"/k/site-a.key","kubeconfig":"/k/site-a.kubeconfig","hostKey":"`+hostKey+`"}`)
	r, versions, err := keptSite(run)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "site-a" || r.Address != "10.0.0.7" || r.JumpHost != "jump" || r.JumpLogin != "root" || r.HostKey != hostKey {
		t.Errorf("read %+v", r)
	}
	if versions["SITE_NVKE_CONTROLLER_VERSION"] != "b93b9fd4aa0a" || versions["KUBERNETES_VERSION"] != "1.36.3" {
		t.Errorf("versions %v", versions)
	}
}

func TestKeptSiteRejects(t *testing.T) {
	for name, record := range map[string]string{
		"no record":       "",
		"not an address":  `{"name":"site-a","address":"node","user":"u","jumpHost":"j","key":"k","kubeconfig":"c"}`,
		"no jump host":    `{"name":"site-a","address":"10.0.0.7","user":"u","key":"k","kubeconfig":"c"}`,
		"no kubeconfig":   `{"name":"site-a","address":"10.0.0.7","user":"u","jumpHost":"j","key":"k"}`,
		"not json at all": `site-a`,
		"a bad host key":  `{"name":"site-a","address":"10.0.0.7","user":"u","jumpHost":"j","key":"k","kubeconfig":"c","hostKey":"ssh-ed25519 nope"}`,
		"no host key":     `{"name":"site-a","address":"10.0.0.7","user":"u","jumpHost":"j","key":"k","kubeconfig":"c"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := keptSite(writeKeptRun(t, record)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestRelayArgs(t *testing.T) {
	args := relayArgs(keptRecord{Address: "10.0.0.7", JumpHost: "jump", JumpLogin: "root"})
	want := []string{
		"ssh", "root@jump",
		`if command -v nc >/dev/null 2>&1; then exec nc 10.0.0.7 22; fi; exec bash -c 'exec 3<>/dev/tcp/10.0.0.7/22 && { cat <&3 & exec cat >&3; }'`,
	}
	if !slices.Equal(args, want) {
		t.Errorf("relayArgs = %q", args)
	}
	if args := relayArgs(keptRecord{Address: "10.0.0.7", JumpHost: "jump"}); args[1] != "jump" {
		t.Errorf("no login leaves tsh its default: %q", args[1])
	}
}

const loopbackKubeconfig = `apiVersion: v1
kind: Config
current-context: admin
contexts:
- name: admin
  context: {cluster: site, user: admin}
clusters:
- name: site
  cluster: {server: "https://127.0.0.1:6443", certificate-authority-data: Q0E=}
users:
- name: admin
  user: {client-certificate-data: Q0VSVA==, client-key-data: S0VZ}
`

func cluster(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	c, err := currentCluster(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestContainerKubeconfigLoopbackServer(t *testing.T) {
	out, err := containerKubeconfig([]byte(loopbackKubeconfig), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	c := cluster(t, out)
	if c["server"] != "https://host.docker.internal:6443" || c["tls-server-name"] != "127.0.0.1" {
		t.Errorf("cluster %v", c)
	}
	if c["certificate-authority-data"] != "Q0E=" {
		t.Error("the CA was dropped")
	}
}

func TestContainerKubeconfigProxyURL(t *testing.T) {
	raw := strings.Replace(loopbackKubeconfig, `server: "https://127.0.0.1:6443"`, `server: "https://proxy.example:443/k8s", proxy-url: "http://127.0.0.1:61234"`, 1)
	out, err := containerKubeconfig([]byte(raw), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	c := cluster(t, out)
	if c["proxy-url"] != "http://host.docker.internal:61234" || c["server"] != "https://proxy.example:443/k8s" {
		t.Errorf("cluster %v", c)
	}
	if _, pinned := c["tls-server-name"]; pinned {
		t.Error("a proxied server keeps its own name; nothing is pinned")
	}
}

func TestContainerKubeconfigLinuxUnchanged(t *testing.T) {
	out, err := containerKubeconfig([]byte(loopbackKubeconfig), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != loopbackKubeconfig {
		t.Error("Linux shares the host's network, so nothing should change")
	}
}

func TestContainerKubeconfigRefusesExec(t *testing.T) {
	raw := strings.Replace(loopbackKubeconfig, "user: {client-certificate-data: Q0VSVA==, client-key-data: S0VZ}", "user: {exec: {command: tsh}}", 1)
	if _, err := containerKubeconfig([]byte(raw), "darwin"); err == nil || !strings.Contains(err.Error(), "exec plugin") {
		t.Errorf("err %v", err)
	}
}

func TestKubeconfigPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.kubeconfig")
	if err := os.WriteFile(path, []byte(loopbackKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	port, err := kubeconfigPort(path)
	if err != nil || port != 6443 {
		t.Errorf("port %d, err %v", port, err)
	}
}

func TestTSHKubeconfig(t *testing.T) {
	log := "Started local proxy for Kubernetes on 127.0.0.1:61234\nexport KUBECONFIG=\"/tmp/tsh-123/kubeconfig.yaml\"\n"
	if got := tshKubeconfig([]byte(log)); got != "/tmp/tsh-123/kubeconfig.yaml" {
		t.Errorf("got %q", got)
	}
}

func TestLabelFilter(t *testing.T) {
	for _, tc := range []struct {
		full, tenant bool
		only, want   string
	}{
		{want: "mgmt-cluster-validate && !creates"},
		{full: true, want: "mgmt-cluster-validate"},
		{tenant: true, want: "(mgmt-cluster-validate || tenant-validate) && !creates"},
		{only: "readiness", want: "(mgmt-cluster-validate && !creates) && (readiness)"},
	} {
		if got := labelFilter(tc.full, tc.only, tc.tenant); got != tc.want {
			t.Errorf("labelFilter(%t, %q, %t) = %q, want %q", tc.full, tc.only, tc.tenant, got, tc.want)
		}
	}
}

func TestPassedEnv(t *testing.T) {
	got := passedEnv([]string{"NVKE_E2E_KEEP_ON_FAILURE=true", "NKE_E2E_TOKEN_NAMESPACE=x", "NVKE_MGMT_CLUSTER_KUBERNETES_VERSION=1", "HOME=/h", "NGC_API_KEY=secret"})
	if !slices.Equal(got, []string{"NKE_E2E_TOKEN_NAMESPACE", "NVKE_E2E_KEEP_ON_FAILURE"}) {
		t.Errorf("got %v", got)
	}
}

func TestRunSwitchesSet(t *testing.T) {
	got := runSwitchesSet([]string{"NVKE_MGMT_CLUSTER_VALIDATE=", "NKE_E2E_AUTH_DISABLE=true", "NVKE_TENANT_VALIDATE=true", "NVKE_MGMT_CLUSTER_VALIDATE_X=1"})
	if !slices.Equal(got, []string{"NKE_E2E_AUTH_DISABLE", "NVKE_MGMT_CLUSTER_VALIDATE"}) {
		t.Errorf("got %v; an empty value still turns a switch off, and adding a tenant is allowed", got)
	}
	t.Setenv("NVKE_E2E_REQUIRE_EXPECTED_VERSION", "false")
	err := runValidate(t.Context(), io.Discard, validateOptions{kubeconfig: "k"})
	if err == nil || !strings.Contains(err.Error(), "NVKE_E2E_REQUIRE_EXPECTED_VERSION set in the environment") {
		t.Errorf("err = %v, want the run refused before it starts", err)
	}
}

func TestDockerArgs(t *testing.T) {
	o := validateOptions{out: "/out", image: "nvke-validate:x", controllerVersion: "v1", kubernetesVersion: "1.36.3", profile: "/p.yaml", dryRun: true}
	args := strings.Join(dockerArgs(o, "linux/arm64", "/out/container.kubeconfig", "mgmt-cluster-validate", []string{"NVKE_E2E_KEEP_ON_FAILURE"}, "darwin"), " ")
	for _, want := range []string{
		"--platform linux/arm64", "-e NVKE_E2E_KEEP_ON_FAILURE ", "-v /out/artifacts:/artifacts",
		"-v /out/container.kubeconfig:/kube/config:ro", "-e NVKE_CONTROLLER_EXPECTED_VERSION=v1",
		"-v /p.yaml:/profile.yaml:ro", "nvke-validate:x -ginkgo.label-filter=mgmt-cluster-validate -ginkgo.dry-run",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--network host") {
		t.Error("Docker Desktop reaches the host through host.docker.internal, not the host network")
	}
	if linux := strings.Join(dockerArgs(o, "linux/amd64", "/k", "f", nil, "linux"), " "); !strings.Contains(linux, "--network host") {
		t.Error("Linux needs the host network to reach the forward")
	}
	if !strings.Contains(args, "-e NVKE_MGMT_CLUSTER_KUBERNETES_VERSION=1.36.3") {
		t.Errorf("args lack the Kubernetes version: %s", args)
	}
	o.controllerVersion, o.kubernetesVersion = "", ""
	if fromProfile := strings.Join(dockerArgs(o, "linux/amd64", "/k", "f", nil, "darwin"), " "); strings.Contains(fromProfile, "VERSION=") {
		t.Errorf("a version left to the profile is not passed: %s", fromProfile)
	}
}

func TestSummarizeJUnit(t *testing.T) {
	report := `<testsuites><testsuite>
<testcase name="[BeforeSuite]"></testcase>
<testcase name="[It] management cluster validation runs every workload"><properties>
  <property name="workloads outside managementCluster.scope, not failing the check" value="&#34;Deployment other-team/broken: 0 of 1 available\nDeployment other-team/b: 0 of 1 available&#34;"></property>
</properties></testcase>
<testcase name="[It] management cluster validation has no pod failing outside its scope"><properties>
  <property name="pods outside managementCluster.scope, not failing the check" value="&#34;other-team/p: CrashLoopBackOff&#34;"></property>
</properties></testcase>
<testcase name="[It] management cluster validation has no pod failing"><failure message="pods: ns/p CrashLoopBackOff&#xA;more"></failure></testcase>
<testcase name="[It] management cluster validation runs Argo CD"><skipped message="managementCluster.components does not list argocd"></skipped></testcase>
<testcase name="[It] management cluster validation issues a certificate"><properties>
  <property name="certificate request denied, not failing the check" value="&#34;ns/r denied (policy)&#34;"></property>
</properties></testcase>
<testcase name="[AfterSuite]"><failure message="cleanup&#xA;more"></failure></testcase>
</testsuite></testsuites>`
	got, passed, err := summarizeJUnit([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	if passed != 3 {
		t.Errorf("passed = %d, want 3", passed)
	}
	for _, want := range []string{
		"3 passed, 1 failed, 1 skipped",
		"FAILED  [AfterSuite]: cleanup, which is the suite's, not a check's\n",
		"certificate request denied, not failing the check:\n    ns/r denied (policy)",
		"FAILED  [It] management cluster validation has no pod failing: pods: ns/p CrashLoopBackOff\n",
		"skipped [It] management cluster validation runs Argo CD: managementCluster.components does not list argocd",
		"workloads outside managementCluster.scope, not failing the check:\n    Deployment other-team/broken: 0 of 1 available\n    Deployment other-team/b",
		"pods outside managementCluster.scope, not failing the check:\n    other-team/p: CrashLoopBackOff",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}

func TestContextOnlyWithKubeconfig(t *testing.T) {
	for _, o := range []validateOptions{
		{runDir: "r", kubeContext: "c"},
		{teleportCluster: "t", kubeContext: "c"},
	} {
		err := runValidate(t.Context(), io.Discard, o)
		if err == nil || !strings.Contains(err.Error(), "--context names a --kubeconfig context") {
			t.Errorf("%+v: err = %v, want --context refused", o, err)
		}
	}
}

func TestCheckHostKey(t *testing.T) {
	node, recorded := newHostKey(t)
	other, _ := newHostKey(t)
	if err := checkHostKey(recorded, node); err != nil {
		t.Errorf("the recorded key: %v", err)
	}
	if err := checkHostKey(recorded, other); err == nil || !strings.Contains(err.Error(), "not the") {
		t.Errorf("another key: err = %v, want it refused", err)
	}
}

// A relay that never answers fails the handshake once its timeout passes,
// rather than hanging the run.
func TestSSHHandshakeTimeout(t *testing.T) {
	defer func(d time.Duration) { sshHandshakeTimeout = d }(sshHandshakeTimeout)
	sshHandshakeTimeout = 200 * time.Millisecond
	client, silent := net.Pipe()
	defer silent.Close()

	start := time.Now()
	_, _, _, err := sshHandshake(client, "10.0.0.1:22", &ssh.ClientConfig{HostKeyCallback: ssh.InsecureIgnoreHostKey()}, func() { _ = client.Close() })
	if err == nil || err.Error() != "no handshake in 200ms" {
		t.Fatalf("err = %v, want the handshake timed out", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s", elapsed)
	}
}
