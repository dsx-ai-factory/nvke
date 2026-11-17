// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func TestKeptSite(t *testing.T) {
	run := writeKeptRun(t, `{"name":"site-a","address":"10.0.0.7","user":"ubuntu","jumpHost":"jump","jumpLogin":"root","key":"/k/site-a.key","kubeconfig":"/k/site-a.kubeconfig"}`)
	r, versions, err := keptSite(run)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "site-a" || r.Address != "10.0.0.7" || r.JumpHost != "jump" || r.JumpLogin != "root" {
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
		{want: "site-validate && !creates"},
		{full: true, want: "site-validate"},
		{tenant: true, want: "(site-validate || tenant-validate) && !creates"},
		{only: "readiness", want: "(site-validate && !creates) && (readiness)"},
	} {
		if got := labelFilter(tc.full, tc.only, tc.tenant); got != tc.want {
			t.Errorf("labelFilter(%t, %q, %t) = %q, want %q", tc.full, tc.only, tc.tenant, got, tc.want)
		}
	}
}

func TestPassedEnv(t *testing.T) {
	got := passedEnv([]string{"NVKE_SITE_SCOPE=namespaces", "NKE_E2E_TOKEN_NAMESPACE=x", "NVKE_SITE_KUBERNETES_VERSION=1", "HOME=/h", "NGC_API_KEY=secret"})
	if !slices.Equal(got, []string{"NKE_E2E_TOKEN_NAMESPACE", "NVKE_SITE_SCOPE"}) {
		t.Errorf("got %v", got)
	}
}

func TestDockerArgs(t *testing.T) {
	o := validateOptions{out: "/out", image: "nvke-validate:x", controllerVersion: "v1", kubernetesVersion: "1.36.3", profile: "/p.yaml", dryRun: true}
	args := strings.Join(dockerArgs(o, "linux/arm64", "/out/container.kubeconfig", "site-validate", []string{"NVKE_SITE_SCOPE"}, "darwin"), " ")
	for _, want := range []string{
		"--platform linux/arm64", "-e NVKE_SITE_SCOPE ", "-v /out/artifacts:/artifacts",
		"-v /out/container.kubeconfig:/kube/config:ro", "-e NVKE_CONTROLLER_EXPECTED_VERSION=v1",
		"-v /p.yaml:/profile.yaml:ro", "nvke-validate:x -ginkgo.label-filter=site-validate -ginkgo.dry-run",
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
}

func TestSummarizeJUnit(t *testing.T) {
	report := `<testsuites><testsuite>
<testcase name="[BeforeSuite]"></testcase>
<testcase name="[It] site validation runs every workload"><properties>
  <property name="workloads outside NVKE_SITE_SCOPE, not failing the check" value="&#34;Deployment other-team/broken: 0 of 1 available\nDeployment other-team/b: 0 of 1 available&#34;"></property>
</properties></testcase>
<testcase name="[It] site validation has no pod failing outside its scope"><properties>
  <property name="pods outside NVKE_SITE_SCOPE, not failing the check" value="&#34;other-team/p: CrashLoopBackOff&#34;"></property>
</properties></testcase>
<testcase name="[It] site validation has no pod failing"><failure message="pods: ns/p CrashLoopBackOff&#xA;more"></failure></testcase>
<testcase name="[It] site validation runs Argo CD"><skipped message="NVKE_SITE_COMPONENTS does not list argocd"></skipped></testcase>
<testcase name="[AfterSuite]"><failure message="cleanup"></failure></testcase>
</testsuite></testsuites>`
	got, err := summarizeJUnit([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2 passed, 1 failed, 1 skipped",
		"FAILED  [It] site validation has no pod failing: pods: ns/p CrashLoopBackOff\n",
		"skipped [It] site validation runs Argo CD: NVKE_SITE_COMPONENTS does not list argocd",
		"workloads outside NVKE_SITE_SCOPE, not failing the check:\n    Deployment other-team/broken: 0 of 1 available\n    Deployment other-team/b",
		"pods outside NVKE_SITE_SCOPE, not failing the check:\n    other-team/p: CrashLoopBackOff",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "cleanup") {
		t.Error("an [AfterSuite] is not a spec")
	}
}
