// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidckubeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func writeAdminKubeconfig(t *testing.T, cfg *clientcmdapi.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.kubeconfig")
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("clientcmd.Write: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write admin kubeconfig: %v", err)
	}
	return path
}

func adminConfig() *clientcmdapi.Config {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["site"] = &clientcmdapi.Cluster{
		Server:                   "https://10.10.50.2:6443",
		CertificateAuthorityData: []byte("CA CERT"),
	}
	cfg.AuthInfos["kubernetes-admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: []byte("CLIENT CERT"),
		ClientKeyData:         []byte("CLIENT KEY"),
	}
	cfg.Contexts["kubernetes-admin@site"] = &clientcmdapi.Context{
		Cluster:  "site",
		AuthInfo: "kubernetes-admin",
	}
	cfg.CurrentContext = "kubernetes-admin@site"
	return cfg
}

func TestGenerateCopiesClusterAndDropsAdminUser(t *testing.T) {
	path := writeAdminKubeconfig(t, adminConfig())

	data, err := Generate(path, Options{
		IssuerURL: "https://idp.example/realms/nke",
		ClientID:  "nvke-cli",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	out, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("Load generated kubeconfig: %v", err)
	}
	cluster := out.Clusters["site"]
	if cluster == nil {
		t.Fatal("generated kubeconfig has no cluster \"site\"")
	}
	if cluster.Server != "https://10.10.50.2:6443" {
		t.Errorf("server = %q", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != "CA CERT" {
		t.Errorf("certificate-authority-data = %q", cluster.CertificateAuthorityData)
	}
	if _, ok := out.AuthInfos["kubernetes-admin"]; ok {
		t.Error("input user was copied")
	}
	user := out.AuthInfos["oidc"]
	if user == nil || user.Exec == nil {
		t.Fatal("generated kubeconfig has no oidc exec user")
	}
	if len(user.ClientCertificateData) != 0 || len(user.ClientKeyData) != 0 || user.Token != "" {
		t.Error("oidc user carries admin credentials")
	}
	if user.Exec.Command != "kubectl" {
		t.Errorf("exec command = %q", user.Exec.Command)
	}
	if user.Exec.APIVersion != "client.authentication.k8s.io/v1" {
		t.Errorf("exec apiVersion = %q", user.Exec.APIVersion)
	}
	wantArgs := []string{
		"oidc-login",
		"get-token",
		"--oidc-issuer-url=https://idp.example/realms/nke",
		"--oidc-client-id=nvke-cli",
	}
	if strings.Join(user.Exec.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("exec args = %#v, want %#v", user.Exec.Args, wantArgs)
	}
	if out.CurrentContext != "oidc@site" {
		t.Errorf("current-context = %q, want oidc@site", out.CurrentContext)
	}
	if _, ok := out.Contexts["kubernetes-admin@site"]; ok {
		t.Error("admin context name was reused")
	}
	ctx := out.Contexts[out.CurrentContext]
	if ctx == nil || ctx.AuthInfo != "oidc" || ctx.Cluster != "site" {
		t.Errorf("context = %+v", ctx)
	}
}

func TestGenerateInlinesCertificateAuthorityFile(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("FROM FILE"), 0o600); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["site"] = &clientcmdapi.Cluster{
		Server:               "https://10.10.50.2:6443",
		CertificateAuthority: "ca.crt",
	}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "admin-token"}
	cfg.Contexts["admin@site"] = &clientcmdapi.Context{Cluster: "site", AuthInfo: "admin"}
	cfg.CurrentContext = "admin@site"
	kcPath := filepath.Join(dir, "admin.kubeconfig")
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.WriteFile(kcPath, data, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	outBytes, err := Generate(kcPath, Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out, err := clientcmd.Load(outBytes)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cluster := out.Clusters["site"]
	if cluster.CertificateAuthority != "" {
		t.Errorf("certificate-authority path leaked: %q", cluster.CertificateAuthority)
	}
	if string(cluster.CertificateAuthorityData) != "FROM FILE" {
		t.Errorf("certificate-authority-data = %q", cluster.CertificateAuthorityData)
	}
}

func TestGenerateKeepsEmptyCAForSystemAuthorities(t *testing.T) {
	cfg := adminConfig()
	cfg.Clusters["site"].CertificateAuthorityData = nil

	data, err := Generate(writeAdminKubeconfig(t, cfg), Options{
		IssuerURL: "https://idp.example",
		ClientID:  "nvke-cli",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cluster := out.Clusters["site"]
	if cluster == nil {
		t.Fatal("generated kubeconfig has no cluster \"site\"")
	}
	if cluster.CertificateAuthority != "" || len(cluster.CertificateAuthorityData) != 0 {
		t.Errorf("CA fields = path %q data %q, want empty for system CAs", cluster.CertificateAuthority, cluster.CertificateAuthorityData)
	}
	if cluster.InsecureSkipTLSVerify {
		t.Error("insecure-skip-tls-verify was set")
	}
}

func TestGenerateAppendsOptionalExecArgs(t *testing.T) {
	path := writeAdminKubeconfig(t, adminConfig())
	data, err := Generate(path, Options{
		IssuerURL:   "https://idp.example",
		ClientID:    "nvke-cli",
		ExtraScopes: []string{"openid", "groups"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	args := strings.Join(out.AuthInfos["oidc"].Exec.Args, " ")
	for _, want := range []string{
		"--oidc-extra-scope=openid",
		"--oidc-extra-scope=groups",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("exec args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "device-code") {
		t.Errorf("default exec args stamped headless grant: %s", args)
	}
}

func TestGenerateHeadlessStampsDeviceCode(t *testing.T) {
	path := writeAdminKubeconfig(t, adminConfig())
	data, err := Generate(path, Options{
		IssuerURL: "https://idp.example",
		ClientID:  "nvke-cli",
		Headless:  true,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := strings.Join(out.AuthInfos["oidc"].Exec.Args, " ")
	want := "oidc-login get-token --oidc-issuer-url=https://idp.example --oidc-client-id=nvke-cli --grant-type=device-code --skip-open-browser"
	if got != want {
		t.Errorf("exec args = %q, want %q", got, want)
	}
}

func TestGenerateRejects(t *testing.T) {
	valid := writeAdminKubeconfig(t, adminConfig())
	tests := map[string]struct {
		path    string
		opts    Options
		wantErr string
	}{
		"empty path": {
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "kubeconfig must not be empty",
		},
		"empty issuer": {
			path:    valid,
			opts:    Options{ClientID: "nvke-cli"},
			wantErr: "oidc-issuer-url must not be empty",
		},
		"empty client id": {
			path:    valid,
			opts:    Options{IssuerURL: "https://idp.example"},
			wantErr: "oidc-client-id must not be empty",
		},
		"missing file": {
			path:    filepath.Join(t.TempDir(), "nope"),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "loading kubeconfig",
		},
		"no current-context": {
			path: writeAdminKubeconfig(t, func() *clientcmdapi.Config {
				cfg := adminConfig()
				cfg.CurrentContext = ""
				return cfg
			}()),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "no current-context",
		},
		"context not found": {
			path: writeAdminKubeconfig(t, func() *clientcmdapi.Config {
				cfg := adminConfig()
				cfg.CurrentContext = "missing"
				return cfg
			}()),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "current-context \"missing\" is not defined",
		},
		"context has no cluster": {
			path: writeAdminKubeconfig(t, func() *clientcmdapi.Config {
				cfg := adminConfig()
				cfg.Contexts[cfg.CurrentContext].Cluster = ""
				return cfg
			}()),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "has no cluster",
		},
		"cluster not found": {
			path: writeAdminKubeconfig(t, func() *clientcmdapi.Config {
				cfg := adminConfig()
				cfg.Contexts[cfg.CurrentContext].Cluster = "missing"
				return cfg
			}()),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "cluster \"missing\" is not defined",
		},
		"cluster has no server": {
			path: writeAdminKubeconfig(t, func() *clientcmdapi.Config {
				cfg := adminConfig()
				cfg.Clusters["site"].Server = ""
				return cfg
			}()),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "has no server",
		},
		"unreadable CA file": {
			path: writeAdminKubeconfig(t, func() *clientcmdapi.Config {
				cfg := adminConfig()
				cfg.Clusters["site"].CertificateAuthorityData = nil
				cfg.Clusters["site"].CertificateAuthority = filepath.Join(t.TempDir(), "missing-ca.crt")
				return cfg
			}()),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "reading certificate-authority",
		},
		"empty CA file": {
			path: writeAdminKubeconfig(t, func() *clientcmdapi.Config {
				dir := t.TempDir()
				ca := filepath.Join(dir, "ca.crt")
				if err := os.WriteFile(ca, nil, 0o600); err != nil {
					t.Fatalf("write empty ca.crt: %v", err)
				}
				cfg := adminConfig()
				cfg.Clusters["site"].CertificateAuthorityData = nil
				cfg.Clusters["site"].CertificateAuthority = ca
				return cfg
			}()),
			opts:    Options{IssuerURL: "https://idp.example", ClientID: "nvke-cli"},
			wantErr: "is empty",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Generate(tc.path, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Generate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
