// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

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

// TestGetKubeconfigCmdOutputFileDoesNotWriteThroughSymlink is the case
// writeKubeconfigFile's rename-based approach exists for: os.WriteFile would
// follow a symlink at outputFile and both leak the credential into its
// target and chmod that target, rather than replacing the symlink itself.
func TestGetKubeconfigCmdOutputFileDoesNotWriteThroughSymlink(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte("fake-kubeconfig-bytes")}
	cmd, _ := newTestKubeconfigCmd(fetcher, nil)

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unrelated-file-outside-output"), 0o644); err != nil {
		t.Fatalf("seeding symlink target: %v", err)
	}
	link := filepath.Join(dir, "kubeconfig-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	cmd.SetArgs([]string{"my-cluster", "--output-file", link})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The symlink must be gone, replaced by a regular file holding the
	// kubeconfig -- not still pointing at target.
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("--output-file left the symlink in place instead of replacing it")
	}
	data, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "fake-kubeconfig-bytes" {
		t.Errorf("link contents = %q, want %q", data, "fake-kubeconfig-bytes")
	}

	// target must be untouched: neither the credential nor a chmod landed on it.
	targetData, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if string(targetData) != "unrelated-file-outside-output" {
		t.Errorf("symlink target was written through: contents = %q", targetData)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(target): %v", err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o644 {
		t.Errorf("symlink target mode = %o, want unchanged %o", got, 0o644)
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

func TestGetKubeconfigCmdOutputFileErrorsWhenDirectoryMissing(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{"my-cluster": []byte("x")}, nil)

	path := filepath.Join(t.TempDir(), "does-not-exist", "kubeconfig")
	cmd.SetArgs([]string{"my-cluster", "--output-file", path})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when the output directory does not exist, got nil")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected no file left behind when the write path errors")
	}
}
