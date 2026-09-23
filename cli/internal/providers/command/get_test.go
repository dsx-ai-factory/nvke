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

	"github.com/dsx-ai-factory/nke/cli/internal/config"
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

func run(t *testing.T, siteFn SiteFn, runner scriptedRunner, args ...string) (string, error) {
	t.Helper()
	cmd := NewGetCmd(siteFn, runner)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
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
		return config.SiteConfig{}, errors.New("no site configured: run 'nkectl config set-site' first")
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
