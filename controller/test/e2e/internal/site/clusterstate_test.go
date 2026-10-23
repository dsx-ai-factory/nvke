// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

// The collector's whole value is that it separates "the controller is not ready" from
// "the controller is ready and the probe could not reach it", so the section that
// answers that has to be present and has to come first.
func TestClusterStateCollectsEndpointsFirst(t *testing.T) {
	script := clusterStateScript()
	endpoints := strings.Index(script, "get service,endpoints,endpointslice")
	if endpoints < 0 {
		t.Fatalf("the script does not read endpoints:\n%s", script)
	}
	for _, later := range []string{"get pods -o wide", "describe pods", "get events"} {
		if i := strings.Index(script, later); i >= 0 && i < endpoints {
			t.Errorf("%q is read before endpoints, which is the section that classifies the failure", later)
		}
	}
}

// Reads only. A collector that mutated the cluster it is reporting on would
// be rewriting the evidence the run is judged on.
func TestClusterStateOnlyReads(t *testing.T) {
	for _, verb := range []string{"delete", "apply", "patch", "edit", "scale", "create", "rollout", "cordon", "drain"} {
		if strings.Contains(clusterStateScript(), " "+verb+" ") {
			t.Errorf("the script runs kubectl %s; it must only read", verb)
		}
	}
}

// Every command is tolerated individually. A cluster with no controller Deployment
// still has events worth reading, and the first failure must not stop them
// being collected.
func TestClusterStateToleratesEachCommand(t *testing.T) {
	script := clusterStateScript()
	stanzas := strings.FieldsFunc(script, func(r rune) bool { return r == ';' || r == '\n' })
	for _, stanza := range stanzas {
		stanza = strings.TrimSpace(stanza)
		if stanza == "" || strings.HasPrefix(stanza, "echo ") {
			continue
		}
		if !strings.HasSuffix(stanza, "|| true") {
			t.Errorf("command is not tolerated, so a failure before it hides the rest: %s", stanza)
		}
	}
}

// The script has to reach the node's shell as one command, because something
// on the way there eats newlines.
//
// Run 37561084841 collected one truncated line for all nine sections:
//
//	===== service and endpointskubectl --kubeconfig ... -o wide
//
// which is what a shell prints when the newlines arrive as backslash
// continuations and are deleted: `echo` swallows the first kubectl invocation
// as its arguments, and `|| trueecho` short-circuits every section after it.
// Rebuilding that transformation over the real script reproduces the artifact
// byte for byte. Quoting survived, so sudo's argument joining is a suspect
// rather than a proven cause -- which is the reason this asserts the script
// survives the transformation rather than asserting what performs it.
func TestClusterStateSurvivesANewlineEatingTransport(t *testing.T) {
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "kubectl"),
		[]byte("#!/bin/sh\necho \"RAN $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Every newline escaped, then removed, exactly as a line continuation.
	var collapsed strings.Builder
	for _, line := range strings.Split(clusterStateScript(), "\n") {
		collapsed.WriteString(line)
	}

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", collapsed.String())
	cmd.Env = append(os.Environ(), "PATH="+stub+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the collapsed script: %v\n%s", err, out)
	}

	sections := []string{
		"service and endpoints", "pods", "deployments", "pod detail", "events",
		"controller logs", "controller logs, previous", "nodes",
		"workloads, all namespaces", "events, all namespaces",
		"cni pod detail", "cni logs", "cni logs, previous",
		"cert-manager pod detail", "cert-manager logs", "cert-manager logs, previous",
	}
	for _, section := range sections {
		if !strings.Contains(string(out), sectionMarker+section+"\n") {
			t.Errorf("section %q did not survive; the node sees:\n%s", section, out)
		}
	}
	if got := strings.Count(string(out), "RAN "); got != len(sections) {
		t.Errorf("%d of %d commands ran; the node sees:\n%s", got, len(sections), out)
	}
}

// A deploy that fails before the controller exists fails in what it waits on:
// the CNI, or cert-manager, which clusterctl init waits for. The controller
// sections are then empty, so the collector has to describe those pods and
// keep the logs of the container that died, or the artifact shows that a pod
// is crash-looping and never why.
func TestClusterStateReadsTheInfrastructureTheDeployWaitsOn(t *testing.T) {
	script := clusterStateScript()
	for _, want := range []string{
		"get events -A",
		"-n 'ovn-kubernetes' describe pods",
		"-n 'ovn-kubernetes' logs -l 'component=network' --all-containers --tail=200 --prefix --ignore-errors --previous",
		"-n 'cert-manager' describe pods",
		"-n 'cert-manager' logs -l 'app.kubernetes.io/instance=cert-manager' --all-containers --tail=200 --prefix --ignore-errors --previous",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script does not run %q:\n%s", want, script)
		}
	}
}

// kubectl logs over a selector stops at the first container it cannot read.
// Run 39238542656 asked ovn-kubernetes for --previous; ovnkube-control-plane
// had never restarted, so the section held that error and not the ovnkube-node
// container that was crash-looping.
func TestClusterStateLogsOutliveAContainerWithNothingToRead(t *testing.T) {
	var reads int
	for _, stanza := range strings.Split(clusterStateScript(), "; ") {
		if !strings.Contains(stanza, " logs -l ") {
			continue
		}
		reads++
		if !strings.Contains(stanza, " --ignore-errors") {
			t.Errorf("a log read stops at the first container it cannot read: %s", stanza)
		}
	}
	if reads == 0 {
		t.Fatal("the script reads no logs")
	}
}

// The collector has to read the namespace the controller is deployed into, which on a
// site cluster is controller.Namespace(). Run 37582097398 dumped every namespace and
// found the controller running there, and found nvke-controller empty.
//
// nvke-controller is where nvkectl's own bootstrap cluster puts it
// (cli/internal/bootstrap/controllerdeploy), so it is the plausible wrong answer, and
// the one a reader reaches for. A collector pointed at it answers "No
// resources found" for every section -- indistinguishable, in the artifact,
// from a cluster with nothing running, which is how run 37582097398 was read
// at first.
//
// The suite's own default is nvke-controller, so the test configures the
// legacy name a site like that one runs under: only then does reading
// nvke-controller mean the collector ignored the configuration.
func TestClusterStateReadsTheNamespaceControllerIsDeployedInto(t *testing.T) {
	t.Setenv(controller.EnvNamespace, "cloud-workflow-engine")
	t.Setenv(controller.EnvService, "cloud-workflow-engine")
	script := clusterStateScript()
	if !strings.Contains(script, "-n "+shellQuote(controller.Namespace())) {
		t.Errorf("the script does not read namespace %s:\n%s", controller.Namespace(), script)
	}
	if strings.Contains(script, "nvke-controller") {
		t.Errorf("the script reads nvke-controller, which is the bootstrap cluster's namespace and empty on a site:\n%s", script)
	}
	if !strings.Contains(script, "app.kubernetes.io/name="+controller.Service()) {
		t.Errorf("the controller log selector does not name %s:\n%s", controller.Service(), script)
	}
}

// The script runs against the kubeconfig on the node, not a forward. If the
// forward is what broke, a collector that needed one would fail exactly where
// the probe did and report nothing.
func TestClusterStateRunsOnTheNode(t *testing.T) {
	script := clusterStateScript()
	if !strings.Contains(script, kubeconfigPath) {
		t.Errorf("the script does not use the node's kubeconfig %s:\n%s", kubeconfigPath, script)
	}
	if strings.Contains(script, "127.0.0.1") {
		t.Error("the script reaches the cluster through a forward, which is the thing that may have failed")
	}
}

func TestCollectClusterState(t *testing.T) {
	session := newFakeSession()
	session.fail = func(string) (string, error) { return "pod/nvke-controller-0  0/1  CrashLoopBackOff", nil }

	dest := filepath.Join(t.TempDir(), "state.txt")
	if err := CollectClusterState(context.Background(), session, dest); err != nil {
		t.Fatalf("CollectClusterState: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "CrashLoopBackOff") {
		t.Errorf("the node's output was not written through: %q", got)
	}
	if session.ran("get service,endpoints,endpointslice") < 0 {
		t.Errorf("no command read endpoints:\n%s", strings.Join(session.runs, "\n"))
	}
}

// A failure to collect still writes what arrived. "kubectl: not found" is an
// answer, and losing it to a non-zero exit status would replace the finding
// with nothing.
func TestCollectClusterStateKeepsPartialOutput(t *testing.T) {
	session := newFakeSession()
	session.fail = func(string) (string, error) {
		return "sh: 1: kubectl: not found", os.ErrPermission
	}

	dest := filepath.Join(t.TempDir(), "state.txt")
	if err := CollectClusterState(context.Background(), session, dest); err != nil {
		t.Fatalf("CollectClusterState: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kubectl: not found", "collection failed"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("want %q in the bundle, got:\n%s", want, got)
		}
	}
}

// The bundle is uploaded as a CI artifact, so it leaves the scope the
// credentials were issued for. kubectl output carries Secrets base64-encoded,
// so both forms have to go.
func TestCollectClusterStateRedacts(t *testing.T) {
	// Not written out as a literal: a base64 blob beside the word "key" is
	// what a credential scanner is built to catch, and this fixture exists
	// precisely to look like one. Encoding it here keeps the test honest and
	// keeps the scanner off a value that was never a credential.
	const secret = "example-token-not-a-credential"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	session := newFakeSession()
	session.fail = func(string) (string, error) {
		return "token: " + secret + "\ndata:\n  key: " + encoded, nil
	}

	dest := filepath.Join(t.TempDir(), "state.txt")
	if err := CollectClusterState(context.Background(), session, dest, secret); err != nil {
		t.Fatalf("CollectClusterState: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), secret) {
		t.Error("the secret survived in the bundle")
	}
	if strings.Contains(string(got), encoded) {
		t.Error("the base64 form of the secret survived in the bundle")
	}
}
