// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/config"
)

// adminKubeconfig is a minimal kubeadm admin.conf. certificate-authority-data
// is "CA CERT"; the client key is "KEY".
const adminKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: my-cluster
  cluster:
    certificate-authority-data: Q0EgQ0VSVA==
    server: https://10.0.0.2:6443
contexts:
- name: kubernetes-admin@my-cluster
  context:
    cluster: my-cluster
    user: kubernetes-admin
current-context: kubernetes-admin@my-cluster
users:
- name: kubernetes-admin
  user:
    client-certificate-data: Q0xJRU5U
    client-key-data: S0VZ
`

// fakeKubeconfigFetcher is a KubeconfigFetcher double keyed by cluster name.
type fakeKubeconfigFetcher map[string][]byte

func (f fakeKubeconfigFetcher) GetClusterKubeconfig(_ context.Context, clusterName string) ([]byte, error) {
	data, ok := f[clusterName]
	if !ok {
		return nil, errors.New("secret not found")
	}
	return data, nil
}

func newTestKubeconfigCmd(fetcher KubeconfigFetcher, siteErr error) (*cobra.Command, *bytes.Buffer) {
	siteFn := func() (config.SiteConfig, error) {
		if siteErr != nil {
			return config.SiteConfig{}, siteErr
		}
		return config.SiteConfig{Kubeconfig: "/dev/null"}, nil
	}
	factory := func(string) (KubeconfigFetcher, error) { return fetcher, nil }

	cmd := NewGetKubeconfigCmd(siteFn, factory)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

func oidcArgs(cluster string, extra ...string) []string {
	args := []string{
		cluster,
		"--oidc-issuer-url", "https://idp.example/realms/nke",
		"--oidc-client-id", "nke-cli",
	}
	return append(args, extra...)
}

func TestGetKubeconfigCmdWritesOIDCKubeconfig(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte(adminKubeconfig)}
	cmd, out := newTestKubeconfigCmd(fetcher, nil)
	cmd.SetArgs(append(oidcArgs("my-cluster"), "--oidc-extra-scope", "groups"))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"https://10.0.0.2:6443",
		"oidc-login",
		"--oidc-issuer-url=https://idp.example/realms/nke",
		"--oidc-client-id=nke-cli",
		"--oidc-extra-scope=groups",
		"oidc@my-cluster",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, leaked := range []string{"kubernetes-admin", "client-key-data", "client-certificate-data"} {
		if strings.Contains(got, leaked) {
			t.Errorf("output contains %q:\n%s", leaked, got)
		}
	}
}

func TestGetKubeconfigCmdWritesOutputFile(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte(adminKubeconfig)}
	cmd, out := newTestKubeconfigCmd(fetcher, nil)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cmd.SetArgs(oidcArgs("my-cluster", "--output-file", path))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "oidc-login") {
		t.Errorf("file contents = %q", data)
	}
	if strings.Contains(string(data), "client-key-data") {
		t.Errorf("file contains the admin key:\n%s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	if !bytes.Contains(out.Bytes(), []byte(path)) {
		t.Errorf("stdout = %q, want the output path", out)
	}
}

func TestGetKubeconfigCmdPropagatesFetchError(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, nil)
	cmd.SetArgs(oidcArgs("missing-cluster"))

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing cluster, got nil")
	}
}

func TestGetKubeconfigCmdPropagatesSiteError(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, errors.New("no site configured"))
	cmd.SetArgs(oidcArgs("my-cluster"))

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when resolving the site fails, got nil")
	}
}

func TestGetKubeconfigCmdRequiresClusterNameArg(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, nil)
	cmd.SetArgs([]string{"--oidc-issuer-url", "https://idp.example", "--oidc-client-id", "nke-cli", "--output-file", "/tmp/x"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when the cluster-name argument is omitted, got nil")
	}
}

func TestGetKubeconfigCmdRequiresOIDCFlags(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte(adminKubeconfig)}
	for _, args := range [][]string{
		{"my-cluster"},
		{"my-cluster", "--oidc-issuer-url", "https://idp.example"},
		{"my-cluster", "--oidc-client-id", "nke-cli"},
	} {
		cmd, _ := newTestKubeconfigCmd(fetcher, nil)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("Execute(%q) succeeded, want a missing-flag error", args)
		}
	}
}

func TestGetKubeconfigCmdOutputFileErrorsWhenDirectoryMissing(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{"my-cluster": []byte(adminKubeconfig)}, nil)

	path := filepath.Join(t.TempDir(), "does-not-exist", "kubeconfig")
	cmd.SetArgs(oidcArgs("my-cluster", "--output-file", path))
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when the output directory does not exist, got nil")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected no file left behind when the write path errors")
	}
}
