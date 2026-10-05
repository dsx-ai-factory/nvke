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

	"github.com/dsx-ai-factory/nke/cli/internal/config"
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

func TestGetKubeconfigCmdWritesOutputFile(t *testing.T) {
	fetcher := fakeKubeconfigFetcher{"my-cluster": []byte("fake-kubeconfig-bytes")}
	cmd, out := newTestKubeconfigCmd(fetcher, nil)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cmd.SetArgs([]string{"my-cluster", "--output-file", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "fake-kubeconfig-bytes" {
		t.Errorf("file contents = %q", data)
	}
	if !bytes.Contains(out.Bytes(), []byte(path)) {
		t.Errorf("stdout = %q, want the output path", out)
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
