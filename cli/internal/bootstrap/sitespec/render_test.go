// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// threeNodes is the smallest valid node list: the validator requires an odd
// count, so three is the smallest that also exercises more than one entry.
func threeNodes() []Node {
	return []Node{
		{Name: "cp-01", IP: "10.10.50.2", TPMEKThumbprint: strings.Repeat("a", 64)},
		{Name: "cp-02", IP: "10.10.50.3"},
		{Name: "cp-03", IP: "10.10.50.4"},
	}
}

// TestExampleMatchesTemplate keeps the shipped example and the embedded
// template from drifting. It compares what they mean rather than their bytes:
// the two are allowed to differ in wording, and pinning bytes would make every
// edit to one a failure in the other.
func TestExampleMatchesTemplate(t *testing.T) {
	fromExample, err := Load(filepath.Join("..", "..", "..", "examples", "sitespec.yaml"))
	if err != nil {
		t.Fatalf("load cli/examples/sitespec.yaml: %v", err)
	}
	fromTemplate, err := decodeBytes(templateYAML)
	if err != nil {
		t.Fatalf("decode cli/internal/bootstrap/sitespec/template.yaml: %v", err)
	}
	fromTemplate.SetDefaults()

	if diff := specDiff(fromTemplate, fromExample); diff != "" {
		t.Errorf("cli/internal/bootstrap/sitespec/template.yaml and cli/examples/sitespec.yaml describe different site specs.\n"+
			"Update both, or the generated file stops matching the documented example.%s", diff)
	}
}

// TestBuildRoundTrip is the zero-edit guarantee: whatever Build validated is
// what Load reads back out of the rendered file. It covers every settable field
// so a value that renders into a commented block cannot pass unnoticed.
func TestBuildRoundTrip(t *testing.T) {
	sets := []string{
		"cluster.k8sVersion=1.33.1",
		"cluster.cniName=calico",
		"cluster.cniVersion=3.31.4",
		"cluster.controlPlaneVIP=10.0.0.5",
		"cluster.podCidr=10.100.0.0/16",
		"cluster.serviceCidr=10.200.0.0/16",
		"cluster.nodeAgentPort=50051",
		"cluster.etcdEncryption=false",
		"cluster.kubeletConfig.systemReservedMemory=8Gi",
		"registry.bootstrapRepo=nvcr.io/example/cwe",
		"registry.bootstrapChart=cloud-workflow-engine",
		"spire.trustDomain=site.example.com",
	}

	built, err := Build("my-tier0-site", threeNodes(), sets)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rendered, err := built.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("the rendered site spec does not load: %v\n%s", err, rendered)
	}

	if diff := specDiff(loaded, built); diff != "" {
		t.Errorf("the rendered site spec does not match what Build validated%s", diff)
	}
	for _, set := range sets {
		path, value, _ := strings.Cut(set, "=")
		if !strings.Contains(string(rendered), value) {
			t.Errorf("--set %s did not reach the rendered file; %q is absent", path, value)
		}
	}
}

// TestRenderKeepsComments guards the reason this renders through yaml.Node
// rather than marshalling a struct: an operator reads and commits this file,
// and the explanations are most of its value.
func TestRenderKeepsComments(t *testing.T) {
	built, err := Build("my-tier0-site", threeNodes(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rendered, err := built.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		"SPDX-License-Identifier: Apache-2.0",
		"CNI plugin",
		"Pod network CIDR",
	} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("rendered site spec lost the comment containing %q:\n%s", want, rendered)
		}
	}
}

// TestRenderOmitsUnsetOptionalFields: an optional field left alone must not
// appear as an empty string. cniVersion empty means "use the agent default",
// which is not the same as cniVersion set to "".
func TestRenderOmitsUnsetOptionalFields(t *testing.T) {
	built, err := Build("my-tier0-site", threeNodes(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rendered, err := built.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, unwanted := range []string{"cniVersion", "controlPlaneVIP", "etcdEncryption", "caCertFile", "caKeyFile", "apiKey"} {
		if line := uncommentedLineFor(string(rendered), unwanted); line != "" {
			t.Errorf("rendered site spec sets %s as %q; an unset optional field should stay a comment:\n%s",
				unwanted, line, rendered)
		}
	}
}

// uncommentedLineFor returns the first line that actually assigns key, ignoring
// the commented-out examples the template carries for every optional field.
func uncommentedLineFor(rendered, key string) string {
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, key+":") {
			return trimmed
		}
	}
	return ""
}

// TestRenderNeverEmitsAPIKey: the environment must not reach the file. Load
// binds REGISTRY_API_KEY because bootstrap honours the operator's shell; a
// generator that did the same would write a live credential into a file the
// whole design says to commit.
func TestRenderNeverEmitsAPIKey(t *testing.T) {
	const secret = "nvapi-not-a-real-key"
	t.Setenv("REGISTRY_API_KEY", secret)

	built, err := Build("my-tier0-site", threeNodes(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rendered, err := built.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(rendered), secret) {
		t.Fatalf("the rendered site spec contains REGISTRY_API_KEY from the environment:\n%s", rendered)
	}
}

// TestBuildIgnoresEnvironment: no bound variable may change the output.
func TestBuildIgnoresEnvironment(t *testing.T) {
	clean, err := Build("my-tier0-site", threeNodes(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cleanRendered, err := clean.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for env := range envBindings() {
		t.Setenv(env, "junk-"+env)
	}
	t.Setenv("NKECTL_CLUSTER_NODES", "someone:10.0.0.9")

	dirty, err := Build("my-tier0-site", threeNodes(), nil)
	if err != nil {
		t.Fatalf("Build with the environment set: %v", err)
	}
	dirtyRendered, err := dirty.Render()
	if err != nil {
		t.Fatalf("Render with the environment set: %v", err)
	}
	if string(cleanRendered) != string(dirtyRendered) {
		t.Errorf("the environment changed the generated site spec:\n--- clean\n%s\n--- with environment set\n%s",
			cleanRendered, dirtyRendered)
	}
}

// TestBuildRejects covers the argument errors, and confirms the field-level
// ones still come from the existing validator rather than a second copy.
func TestBuildRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		clusterName string
		nodes       []Node
		sets        []string
		wantSub     string
	}{
		"unknown --set path":     {"s", threeNodes(), []string{"cluster.nope=1"}, "unknown field"},
		"--set without a value":  {"s", threeNodes(), []string{"cluster.podCidr"}, "expected path=value"},
		"--set the api key":      {"s", threeNodes(), []string{"registry.apiKey=x"}, "committed"},
		"--set the node list":    {"s", threeNodes(), []string{"cluster.nodes=x"}, "--nodes"},
		"unsupported cni":        {"s", threeNodes(), []string{"cluster.cniName=cilium"}, pathCniName},
		"bad cidr":               {"s", threeNodes(), []string{"cluster.podCidr=nope"}, pathPodCidr},
		"cluster name not label": {"Not_A_Label", threeNodes(), nil, pathClusterName},
		"even node count":        {"s", threeNodes()[:2], nil, pathNodes},
		"bad node ip":            {"s", []Node{{Name: "a", IP: "999.1.1.1"}}, nil, "ip"},
		"bad thumbprint":         {"s", []Node{{Name: "a", IP: "10.0.0.1", TPMEKThumbprint: "xyz"}}, nil, "tpmEkThumbprint"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Build(tc.clusterName, tc.nodes, tc.sets)
			if err == nil {
				t.Fatalf("Build accepted invalid input; want an error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestSettablePathsExcludesDenied pins the derivation: every bound field is
// settable unless it is denied on purpose, so a new binding does not become
// silently unreachable.
func TestSettablePathsExcludesDenied(t *testing.T) {
	settable := map[string]bool{}
	for _, p := range SettablePaths() {
		settable[p] = true
	}
	for _, path := range envBindings() {
		_, denied := deniedPaths()[path]
		if denied && settable[path] {
			t.Errorf("%s is denied but appears in SettablePaths()", path)
		}
		if !denied && !settable[path] {
			t.Errorf("%s is bound to an environment variable but is not settable and is not denied", path)
		}
	}
}

func TestParseNodesCSV(t *testing.T) {
	const header = "name,ip,tpm_ek_thumbprint\n"

	t.Run("valid", func(t *testing.T) {
		got, err := ParseNodesCSV("hosts.csv", strings.NewReader(
			header+
				"cp-01,10.10.50.2,"+strings.Repeat("a", 64)+"\n"+
				"cp-02,10.10.50.3,\n"+
				"cp-03,10.10.50.4,\n",
		))
		if err != nil {
			t.Fatalf("ParseNodesCSV: %v", err)
		}
		want := []Node{
			{Name: "cp-01", IP: "10.10.50.2", TPMEKThumbprint: strings.Repeat("a", 64)},
			{Name: "cp-02", IP: "10.10.50.3"},
			{Name: "cp-03", IP: "10.10.50.4"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("column order does not matter", func(t *testing.T) {
		got, err := ParseNodesCSV("hosts.csv", strings.NewReader("ip,name\n10.0.0.1,cp-01\n"))
		if err != nil {
			t.Fatalf("ParseNodesCSV: %v", err)
		}
		if got[0].Name != "cp-01" || got[0].IP != "10.0.0.1" {
			t.Errorf("got %+v, want name=cp-01 ip=10.0.0.1", got[0])
		}
	})

	t.Run("byte-order mark is tolerated", func(t *testing.T) {
		if _, err := ParseNodesCSV("hosts.csv", strings.NewReader("\ufeffname,ip\ncp-01,10.0.0.1\n")); err != nil {
			t.Errorf("a spreadsheet-exported CSV should parse: %v", err)
		}
	})

	for name, tc := range map[string]struct{ in, wantSub string }{
		"empty file":       {"", "empty"},
		"header only":      {header, "no node rows"},
		"unknown column":   {"name,ip,colour\ncp-01,10.0.0.1,red\n", "unknown column"},
		"missing name":     {"ip\n10.0.0.1\n", `missing the "name" column`},
		"missing ip":       {"name\ncp-01\n", `missing the "ip" column`},
		"duplicate column": {"name,ip,name\ncp-01,10.0.0.1,x\n", "more than once"},
		"short row":        {header + "cp-01,10.0.0.1\n", "field"},
		"empty name":       {header + ",10.0.0.1,\n", "name is empty"},
		"empty ip":         {header + "cp-01,,\n", "ip is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNodesCSV("hosts.csv", strings.NewReader(tc.in))
			if err == nil {
				t.Fatalf("ParseNodesCSV accepted invalid input; want an error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
			if !strings.Contains(err.Error(), "hosts.csv") {
				t.Errorf("error %q does not name the file the operator wrote", err)
			}
		})
	}
}

// TestRenderPlacesOptionalFieldsInPosition covers the fields the template
// carries only as commented-out examples. Each has to land where the example
// stood, keeping the prose that explains it — appending instead would strand
// the explanation above whatever followed and put the field at the end of the
// file, away from the section it belongs to.
func TestRenderPlacesOptionalFieldsInPosition(t *testing.T) {
	built, err := Build("my-tier0-site", threeNodes(), []string{
		"cluster.cniVersion=3.30.1",
		"cluster.controlPlaneVIP=10.0.0.5",
		"cluster.nodeAgentPort=50051",
		"spire.trustDomain=site.example.com",
		"spire.caCertFile=/etc/nke/ca.crt",
		"spire.caKeyFile=/etc/nke/ca.key",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rendered, err := built.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := string(rendered)

	// Ordering: each optional field sits inside the section it belongs to.
	for _, order := range [][2]string{
		{"cniName:", "cniVersion:"},
		{"cniVersion:", "podCidr:"},
		{"serviceCidr:", "controlPlaneVIP:"},
		{"controlPlaneVIP:", "nodeAgentPort:"},
		{"nodeAgentPort:", "kubeletConfig:"},
		{"cluster:", "spire:"},
		{"spire:", "registry:"},
	} {
		before, after := strings.Index(out, order[0]), strings.Index(out, order[1])
		if before < 0 || after < 0 || before > after {
			t.Errorf("%q should appear before %q:\n%s", order[0], order[1], out)
		}
	}

	// The prose introducing each example moves with it rather than being left
	// above the next section.
	spire := strings.Index(out, "\nspire:")
	identity := strings.Index(out, "Identity plane")
	if identity < 0 || identity > spire {
		t.Errorf("the SPIRE explanation should introduce spire:, not follow it:\n%s", out)
	}
	registry := strings.Index(out, "\nregistry:")
	if c := strings.Index(out, "Container registry for the bootstrap"); c < 0 || c > registry || c < spire {
		t.Errorf("registry should keep its own comment, between spire: and registry::\n%s", out)
	}

	// The example is replaced, not duplicated.
	for _, key := range []string{"cniVersion", "controlPlaneVIP", "nodeAgentPort", "trustDomain", "caCertFile"} {
		if strings.Contains(out, "# "+key+":") {
			t.Errorf("the commented-out example of %s survived alongside the real value:\n%s", key, out)
		}
	}
}

// TestRenderEtcdEncryptionIsABool: the field is a YAML bool, and the rendered
// file has to say `false`, not `"false"`. A quoted string does not decode into
// the struct, so a wrong style here is a generated file that does not load.
func TestRenderEtcdEncryptionIsABool(t *testing.T) {
	built, err := Build("my-tier0-site", threeNodes(), []string{"cluster.etcdEncryption=false"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built.Cluster.EtcdEncryptionEnabled() {
		t.Fatal("--set cluster.etcdEncryption=false left encryption enabled")
	}
	rendered, err := built.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if line := uncommentedLineFor(string(rendered), "etcdEncryption"); line != "etcdEncryption: false" {
		t.Errorf("rendered line = %q, want %q:\n%s", line, "etcdEncryption: false", rendered)
	}

	// Explicitly on is the default, so it stays a comment like any other
	// optional field left at its default.
	built, err = Build("my-tier0-site", threeNodes(), []string{"cluster.etcdEncryption=true"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rendered, err = built.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if line := uncommentedLineFor(string(rendered), "etcdEncryption"); line != "" {
		t.Errorf("explicit true rendered as %q; the default should stay a comment", line)
	}

	_, err = Build("my-tier0-site", threeNodes(), []string{"cluster.etcdEncryption=maybe"})
	if err == nil || !strings.Contains(err.Error(), "cluster.etcdEncryption") || !strings.Contains(err.Error(), "maybe") {
		t.Errorf("Build with a non-boolean value: err = %v, want one naming the path and the value", err)
	}
}
