// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/providers"
	"github.com/dsx-ai-factory/nvke/cli/internal/providers/credentials"
)

// scriptedRunner answers kubectl calls by matching on the arguments, so a test
// cannot pass while the command asks the cluster a different question.
type scriptedRunner struct {
	responses map[string]string
}

func (s scriptedRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	for match, out := range s.responses {
		if strings.Contains(joined, match) {
			return out, nil
		}
	}
	return "", errors.New("scriptedRunner: no response for: " + joined)
}

// siteWithKubeconfig returns a SiteFn pointing at a throwaway kubeconfig.
func siteWithKubeconfig(t *testing.T) SiteFn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return func() (config.SiteConfig, error) {
		return config.SiteConfig{URL: "http://localhost:8080", Kubeconfig: path}, nil
	}
}

// emptyCluster reports a site controller with no providers installed.
func emptyCluster() scriptedRunner {
	return scriptedRunner{responses: map[string]string{
		"get crd":         `{"items":[]}`,
		"get deployments": `{"items":[]}`,
	}}
}

// unusedLister is a lister factory for clusters that serve no Identity CRD,
// where the command must not build a client to read Identities.
func unusedLister(t *testing.T) credentials.ListerFactory {
	t.Helper()
	return func(string) (credentials.Lister, error) {
		t.Error("built an Identity lister although no Identity CRD is established")
		return nil, errors.New("unexpected")
	}
}

// fixedIdentities is a Lister that always returns the same Identities.
type fixedIdentities []credentials.Identity

func (f fixedIdentities) List(context.Context, providers.IdentityKind, string) ([]credentials.Identity, error) {
	return f, nil
}

// serving returns a lister factory that serves ids.
func serving(ids ...credentials.Identity) credentials.ListerFactory {
	return func(string) (credentials.Lister, error) { return fixedIdentities(ids), nil }
}

// nicoDefault returns the NicoIdentity naming nico-credentials in
// capnico-system, last checked age ago with the given Ready result.
func nicoDefault(age time.Duration, status, reason, message string) credentials.Identity {
	return credentials.Identity{
		Namespace:   "capnico-system",
		Name:        "nico-default",
		Generation:  1,
		Created:     time.Now().Add(-time.Hour),
		SecretName:  "nico-credentials",
		Ready:       &credentials.Condition{Status: status, Reason: reason, Message: message, ObservedGeneration: 1},
		LastChecked: new(time.Now().Add(-age)),
	}
}

// established renders a CRD list in which every named CRD is established.
func established(names ...string) string {
	items := make([]string, 0, len(names))
	for _, n := range names {
		items = append(items, `{"metadata":{"name":"`+n+`"},"spec":{"versions":[{"served":true}]},`+
			`"status":{"conditions":[{"type":"Established","status":"True"}]}}`)
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// nicoSite reports a site where CAPNICo, its Identity CRD, an available
// manager and its credentials Secret are all present.
func nicoSite() scriptedRunner {
	return scriptedRunner{responses: map[string]string{
		"get crd": established(
			"nicoclusters.infrastructure.cluster.x-k8s.io",
			"nicomachinetemplates.infrastructure.cluster.x-k8s.io",
			"nicoidentities.infrastructure.cluster.x-k8s.io",
		),
		"get deployments": `{"items":[{"metadata":{"name":"controller-manager","namespace":"capnico-system",` +
			`"labels":{"app.kubernetes.io/name":"cluster-api-provider-nico"},"generation":1},` +
			`"spec":{"template":{"spec":{"containers":[{"args":[]}]}}},` +
			`"status":{"observedGeneration":1,"readyReplicas":1,"replicas":1,` +
			`"conditions":[{"type":"Available","status":"True"}]}}]}`,
		"get secret": "secret/nico-credentials\n",
	}}
}

func run(t *testing.T, siteFn SiteFn, runner scriptedRunner, args ...string) (string, error) {
	t.Helper()
	cmd := NewGetCmd(siteFn, runner, unusedLister(t))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// runSplit runs the command with newLister and keeps stdout and stderr apart.
func runSplit(t *testing.T, runner scriptedRunner, newLister credentials.ListerFactory, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewGetCmd(siteWithKubeconfig(t), runner, newLister)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// row returns the table line for provider, or fails the test.
func row(t *testing.T, table, provider string) string {
	t.Helper()
	for line := range strings.SplitSeq(table, "\n") {
		if strings.HasPrefix(line, provider+" ") {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", provider, table)
	return ""
}

// TestGetProviders_EmptyClusterListsEverythingAsNotInstalled is the behaviour
// that makes the command usable on a site where nothing is installed yet:
// every provider is reported, and none of it is an error.
func TestGetProviders_EmptyClusterListsEverythingAsNotInstalled(t *testing.T) {
	out, err := run(t, siteWithKubeconfig(t), emptyCluster())
	if err != nil {
		t.Fatalf("get providers failed on an empty cluster: %v", err)
	}
	if !strings.Contains(out, "PROVIDER") || !strings.Contains(out, "STATE") {
		t.Errorf("output has no header row:\n%s", out)
	}
	for _, want := range []string{"nico", "launchlayer"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "NOT-INSTALLED") != 2 {
		t.Errorf("want both providers NOT-INSTALLED:\n%s", out)
	}
}

func TestGetProviders_JSONOutputParses(t *testing.T) {
	out, err := run(t, siteWithKubeconfig(t), emptyCluster(), "-o", "json")
	if err != nil {
		t.Fatalf("get providers -o json: %v", err)
	}
	var got []struct {
		Type  string `json:"type"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("got %d providers, want 2", len(got))
	}
	for _, g := range got {
		if g.State != "NOT-INSTALLED" {
			t.Errorf("%s state = %q, want NOT-INSTALLED", g.Type, g.State)
		}
	}
}

func TestGetProviders_RejectsUnknownOutputFormat(t *testing.T) {
	_, err := run(t, siteWithKubeconfig(t), emptyCluster(), "-o", "toml")
	if err == nil {
		t.Fatal("accepted an unknown -o value")
	}
	if !strings.Contains(err.Error(), "toml") {
		t.Errorf("error = %q, want it to name the rejected format", err)
	}
}

func TestGetProviders_RejectsPositionalArgs(t *testing.T) {
	if _, err := run(t, siteWithKubeconfig(t), emptyCluster(), "nico"); err == nil {
		t.Fatal("accepted a positional argument")
	}
}

// TestGetProviders_NoKubeconfigNamesTheFix covers the site registered before
// kubeconfigs were read by anything, so the field may be empty.
func TestGetProviders_NoKubeconfigNamesTheFix(t *testing.T) {
	siteFn := func() (config.SiteConfig, error) {
		return config.SiteConfig{URL: "http://localhost:8080"}, nil
	}
	_, err := run(t, siteFn, emptyCluster())
	if err == nil {
		t.Fatal("succeeded with no kubeconfig configured")
	}
	if !strings.Contains(err.Error(), "config set-site") {
		t.Errorf("error = %q, want it to name the command that fixes it", err)
	}
}

// TestGetProviders_SiteResolutionFailurePropagates covers a machine with no
// site configured at all.
func TestGetProviders_SiteResolutionFailurePropagates(t *testing.T) {
	siteFn := func() (config.SiteConfig, error) {
		return config.SiteConfig{}, errors.New("no site configured: run 'nvkectl config set-site' first")
	}
	_, err := run(t, siteFn, emptyCluster())
	if err == nil {
		t.Fatal("succeeded with no site configured")
	}
	if !strings.Contains(err.Error(), "no site configured") {
		t.Errorf("error = %q, want the site resolution error", err)
	}
}

// TestGetProviders_ReportsDetailForDegraded checks that the state operators
// have to act on also says what to look at.
func TestGetProviders_ReportsDetailForDegraded(t *testing.T) {
	runner := scriptedRunner{responses: map[string]string{
		"get crd": `{"items":[` +
			`{"metadata":{"name":"nicoclusters.infrastructure.cluster.x-k8s.io"},` +
			`"spec":{"versions":[{"served":true}]},` +
			`"status":{"conditions":[{"type":"Established","status":"True"}]}},` +
			`{"metadata":{"name":"nicomachinetemplates.infrastructure.cluster.x-k8s.io"},` +
			`"spec":{"versions":[{"served":true}]},` +
			`"status":{"conditions":[{"type":"Established","status":"True"}]}}]}`,
		"get deployments": `{"items":[]}`,
		"get secret":      "secret/nico-credentials\n",
	}}

	out, err := run(t, siteWithKubeconfig(t), runner)
	if err != nil {
		t.Fatalf("get providers: %v", err)
	}
	if !strings.Contains(out, "DEGRADED") {
		t.Errorf("want nico DEGRADED:\n%s", out)
	}
	if !strings.Contains(out, "no manager Deployment found") {
		t.Errorf("DEGRADED row carries no actionable detail:\n%s", out)
	}
}

// TestGetProviders_HelpDoesNotNeedASite keeps --help working on a machine that
// has never run config set-site.
func TestGetProviders_HelpDoesNotNeedASite(t *testing.T) {
	siteFn := func() (config.SiteConfig, error) {
		t.Fatal("--help resolved the site")
		return config.SiteConfig{}, nil
	}
	if _, err := run(t, siteFn, emptyCluster(), "--help"); err != nil {
		t.Fatalf("--help failed: %v", err)
	}
}

func TestGetProviders_ShowsCredentialHealthColumns(t *testing.T) {
	out, _, err := runSplit(t, nicoSite(), serving(nicoDefault(2*time.Minute, "True", "ValidationSucceeded",
		"Authentication and current-tenant lookup succeeded.")))
	if err != nil {
		t.Fatalf("get providers: %v", err)
	}
	first, _, _ := strings.Cut(out, "\n")
	header := strings.Fields(first)
	if strings.Join(header, " ") != "PROVIDER STATE REPLICAS CREDENTIALS LAST CHECK DETAIL" {
		t.Errorf("header = %v", header)
	}
	nico := strings.Fields(row(t, out, "nico"))
	if len(nico) < 6 || nico[1] != "CONFIGURED" || nico[3] != "HEALTHY" || nico[4] != "2m" {
		t.Errorf("nico row = %v, want CONFIGURED, HEALTHY, checked 2m ago", nico)
	}
	if !strings.Contains(row(t, out, "nico"), "capnico-system/nico-default: Authentication and current-tenant lookup succeeded.") {
		t.Errorf("nico DETAIL does not name the Identity and its message:\n%s", out)
	}
	ll := strings.Fields(row(t, out, "launchlayer"))
	if len(ll) < 5 || ll[3] != "-" || ll[4] != "-" {
		t.Errorf("launchlayer row = %v, want - in CREDENTIALS and LAST CHECK", ll)
	}
}

func TestGetProviders_JSONShowsTheNextCheckTimeAndNoStaleThreshold(t *testing.T) {
	id := nicoDefault(2*time.Minute, "True", "ValidationSucceeded", "Authentication and current-tenant lookup succeeded.")
	id.NextCheck = new(id.LastChecked.Add(4*time.Minute + 30*time.Second))
	out, stderr, err := runSplit(t, nicoSite(), serving(id), "-o", "json")
	if err != nil {
		t.Fatalf("get providers -o json: %v\n%s", err, stderr)
	}
	var listed []struct {
		Type        string         `json:"type"`
		Credentials map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	for _, p := range listed {
		if p.Type != "nico" {
			continue
		}
		if v, ok := p.Credentials["staleAfterSeconds"]; ok {
			t.Errorf("credentials carries staleAfterSeconds = %v, want no such field", v)
		}
		// The API reports whole seconds, as the provider writes them.
		want := id.NextCheck.UTC().Truncate(time.Second).Format(time.RFC3339)
		if got, ok := p.Credentials["nextCheckTime"].(string); !ok || got != want {
			t.Errorf("credentials.nextCheckTime = %v, want %q", p.Credentials["nextCheckTime"], want)
		}
		return
	}
	t.Fatalf("no nico provider in:\n%s", out)
}
