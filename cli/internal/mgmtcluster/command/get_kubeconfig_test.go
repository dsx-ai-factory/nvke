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
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
)

type fakeKubeconfigFetcher struct {
	admin    map[string][]byte
	adminErr error
}

func (f fakeKubeconfigFetcher) GetClusterKubeconfig(_ context.Context, clusterName string) ([]byte, error) {
	if f.adminErr != nil {
		return nil, f.adminErr
	}
	data, ok := f.admin[clusterName]
	if !ok {
		return nil, errors.New("admin kubeconfig secret not found")
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

func adminKubeconfigBytes(t *testing.T) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["tenant"] = &clientcmdapi.Cluster{
		Server:                   "https://10.10.50.2:6443",
		CertificateAuthorityData: []byte("ADMIN CA"),
	}
	cfg.AuthInfos["kubernetes-admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: []byte("CLIENT CERT"),
		ClientKeyData:         []byte("CLIENT KEY"),
	}
	cfg.Contexts["kubernetes-admin@tenant"] = &clientcmdapi.Context{
		Cluster:  "tenant",
		AuthInfo: "kubernetes-admin",
	}
	cfg.CurrentContext = "kubernetes-admin@tenant"
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return data
}

func oidcArgs(extra ...string) []string {
	args := []string{
		"my-cluster",
		"--oidc-issuer-url", "https://idp.example/realms/nke",
		"--oidc-client-id", "nke-cli",
	}
	return append(args, extra...)
}

func readyFetcher(t *testing.T) fakeKubeconfigFetcher {
	t.Helper()
	return fakeKubeconfigFetcher{
		admin: map[string][]byte{"my-cluster": adminKubeconfigBytes(t)},
	}
}

func TestGetKubeconfigCmdWritesOIDCKubeconfig(t *testing.T) {
	cmd, out := newTestKubeconfigCmd(readyFetcher(t), nil)
	cmd.SetArgs(oidcArgs())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, err := clientcmd.Load(out.Bytes())
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, out)
	}
	cluster := got.Clusters["tenant"]
	if cluster == nil || cluster.Server != "https://10.10.50.2:6443" {
		t.Fatalf("cluster = %+v", cluster)
	}
	if string(cluster.CertificateAuthorityData) != "ADMIN CA" {
		t.Errorf("CA = %q", cluster.CertificateAuthorityData)
	}
	if _, ok := got.AuthInfos["kubernetes-admin"]; ok {
		t.Error("admin user was copied")
	}
	if bytes.Contains(out.Bytes(), []byte("CLIENT CERT")) || bytes.Contains(out.Bytes(), []byte("CLIENT KEY")) {
		t.Error("admin client material leaked")
	}
	user := got.AuthInfos["oidc"]
	if user == nil || user.Exec == nil || user.Exec.Command != "kubectl" {
		t.Fatalf("oidc user = %+v", user)
	}
}

func TestGetKubeconfigCmdWritesOutputFile(t *testing.T) {
	cmd, out := newTestKubeconfigCmd(readyFetcher(t), nil)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cmd.SetArgs(oidcArgs("--output-file", path))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := clientcmd.Load(data); err != nil {
		t.Fatalf("Load file: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte(path)) {
		t.Errorf("stdout = %q, want the output path", out)
	}
}

func TestGetKubeconfigCmdRequiresOIDCFlags(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, nil)
	cmd.SetArgs([]string{"my-cluster"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when OIDC flags are omitted, got nil")
	}
}

func TestGetKubeconfigCmdPropagatesFetchError(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(fakeKubeconfigFetcher{}, nil)
	cmd.SetArgs(oidcArgs())

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing cluster, got nil")
	}
}

func TestGetKubeconfigCmdPropagatesSiteError(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(readyFetcher(t), errors.New("no site configured"))
	cmd.SetArgs(oidcArgs())

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when resolving the site fails, got nil")
	}
}

func TestGetKubeconfigCmdRequiresClusterNameArg(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(readyFetcher(t), nil)
	cmd.SetArgs([]string{"--oidc-issuer-url", "https://idp.example", "--oidc-client-id", "nke-cli"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when the cluster-name argument is omitted, got nil")
	}
}

func TestGetKubeconfigCmdOutputFileErrorsWhenDirectoryMissing(t *testing.T) {
	cmd, _ := newTestKubeconfigCmd(readyFetcher(t), nil)

	path := filepath.Join(t.TempDir(), "does-not-exist", "kubeconfig")
	cmd.SetArgs(oidcArgs("--output-file", path))
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when the output directory does not exist, got nil")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected no file left behind when the write path errors")
	}
}
