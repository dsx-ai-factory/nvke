// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/config"
)

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

func TestGetKubeconfigCmdWritesToOutOrStdout(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte("fake-kubeconfig-bytes")}
	cmd, out := newTestKubeconfigCmd(fetcher, nil)
	cmd.SetArgs([]string{"my-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.String() != "fake-kubeconfig-bytes" {
		t.Errorf("captured output = %q, want %q", out.String(), "fake-kubeconfig-bytes")
	}
}

func TestGetKubeconfigCmdOutputFileResetsPermissionsOnExistingFile(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte("fake-kubeconfig-bytes")}
	cmd, _ := newTestKubeconfigCmd(fetcher, nil)

	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	// Pre-existing file with a permissive mode, as if it were a stale
	// world-readable kubeconfig from an earlier, unrelated write.
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seeding existing file: %v", err)
	}

	cmd.SetArgs([]string{"my-cluster", "--output-file", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want %o (credentials must not stay world/group readable)", got, 0o600)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "fake-kubeconfig-bytes" {
		t.Errorf("file contents = %q, want %q", data, "fake-kubeconfig-bytes")
	}
}

func TestGetKubeconfigCmdOutputFileSetsPermissionsOnNewFile(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte("fake-kubeconfig-bytes")}
	cmd, _ := newTestKubeconfigCmd(fetcher, nil)

	path := filepath.Join(t.TempDir(), "kubeconfig")
	cmd.SetArgs([]string{"my-cluster", "--output-file", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want %o", got, 0o600)
	}
}

func TestGetKubeconfigCmdPropagatesFetchError(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, nil)
	cmd.SetArgs([]string{"missing-cluster"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing cluster, got nil")
	}
}

func TestGetKubeconfigCmdPropagatesSiteError(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, errors.New("no site configured"))
	cmd.SetArgs([]string{"my-cluster"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when resolving the site fails, got nil")
	}
}

func TestGetKubeconfigCmdRequiresClusterNameArg(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, nil)
	cmd.SetArgs([]string{"--output-file", "/tmp/x"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when the cluster-name argument is omitted, got nil")
	}
}
