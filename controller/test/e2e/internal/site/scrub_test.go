// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"encoding/base64"
	"strings"
	"testing"
)

const sixtyFourHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Every shape the scrub knows, with the value that must be gone afterwards,
// and the text that must be left alone: a pod spec's `secret:` volume, a
// kubeadm ClusterConfiguration, a container digest and a pod name are the
// things a reader needs and a careless pattern eats.
func TestScrubPatterns(t *testing.T) {
	patterns := scrubPatterns()
	// Built at run time: a committed JWT, token or join-token literal reads
	// as a leaked credential to the Secrets gate, whatever its value.
	fakeToken := strings.Repeat("t", 20)
	fakeJoinToken := "00000000-0000-4000-8000-" + strings.Repeat("0", 12)
	fakeJWTPayload := "eyJ" + strings.Repeat("p", 12)
	fakeJWT := "eyJ" + strings.Repeat("h", 12) + "." + fakeJWTPayload + "." + strings.Repeat("s", 12)
	// base64(user:password), which is not the password and not its base64,
	// so no known-value needle holds it.
	fakeBasic := base64.StdEncoding.EncodeToString([]byte("nvke:" + strings.Repeat("p", 12)))
	cases := []struct {
		name, in, gone string
	}{
		{
			name: "kubeadm join line",
			in: "kubeadm join 10.0.0.5:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:" + sixtyFourHex +
				" --certificate-key " + sixtyFourHex,
			gone: "abcdef.0123456789abcdef",
		},
		{name: "certificate key", in: "--certificate-key " + sixtyFourHex, gone: sixtyFourHex},
		{name: "ca cert hash", in: "--discovery-token-ca-cert-hash sha256:" + sixtyFourHex, gone: sixtyFourHex},
		{name: "json args", in: `{"args":["--password=hunter2hunter2","--api-key=nvapi-ABCDEFGHIJKLMNOPQRSTUVWXYZ"]}`, gone: "hunter2hunter2"},
		{name: "nvapi key", in: "NGC_API_KEY=nvapi-ABCDEFGHIJKLMNOPQRSTUVWXYZ", gone: "ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
		{name: "json token", in: `{"token":"` + fakeToken + `"}`, gone: fakeToken},
		{name: "yaml password", in: "password: s3cretpassw0rd\n", gone: "s3cretpassw0rd"},
		{name: "client key data", in: "client-key-data: LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQo=\n", gone: "LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQo="},
		{name: "postgres url", in: "postgres://nvke:s3cretpassw0rd@postgres-postgresql.postgres.svc:5432/nvke", gone: "s3cretpassw0rd"},
		{name: "bearer", in: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789", gone: "abcdefghijklmnopqrstuvwxyz0123456789"},
		{name: "jwt", in: "id_token=" + fakeJWT, gone: fakeJWTPayload},
		{name: "spire join token flag", in: "spire-agent run -joinToken " + fakeJoinToken + " -config /etc/spire-agent/agent.conf", gone: fakeJoinToken},
		{name: "spire join token key", in: "SPIRE_JOIN_TOKEN=" + fakeJoinToken, gone: fakeJoinToken},
		{name: "hosts.toml header table", in: "[host.\"https://registry\".header]\n  Authorization = [\"Basic " + fakeBasic + "\"]\n", gone: fakeBasic},
		{name: "hosts.toml inline header", in: "header = {Authorization = \"Basic " + fakeBasic + "\"}\n", gone: fakeBasic},
		{name: "http basic header", in: "Authorization: Basic " + fakeBasic + "\n", gone: fakeBasic},
		{name: "proxy basic header", in: "Proxy-Authorization: Basic " + fakeBasic, gone: fakeBasic},
		{name: "basic credential without its header", in: "auth header was Basic " + fakeBasic + " for the pull", gone: fakeBasic},
		{name: "containerd auth", in: "  auth = \"" + fakeBasic + "\"\n", gone: fakeBasic},
		{name: "containerd identitytoken", in: "  identitytoken = \"" + fakeToken + "\"\n", gone: fakeToken},
		{name: "registry token", in: "registrytoken: " + fakeToken + "\n", gone: fakeToken},
		{name: "secret key", in: "secret_key = " + fakeToken + "\n", gone: fakeToken},
		{
			name: "pem private key",
			in:   "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0Z3VS5JJcds3xfn\n-----END RSA PRIVATE KEY-----\n",
			gone: "MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(scrubBytes([]byte(tc.in), patterns))
			if strings.Contains(out, tc.gone) {
				t.Errorf("still present after scrubbing:\n%s", out)
			}
			if findings := verifyClean("m", []byte(out), patterns, nil); len(findings) > 0 {
				t.Errorf("the scrubbed text does not verify clean: %v\n%s", findings, out)
			}
		})
	}

	for _, keep := range []string{
		"volumes:\n- name: certs\n  secret:\n    secretName: spire-ca\n",
		"clusterName: site\nkubernetesVersion: v1.36.3\ncontrolPlaneEndpoint: 10.0.0.5:6443\n",
		"image: localhost/nvke/ovnkube@sha256:" + sixtyFourHex + "\n",
		"kube-system coredns-559f6c778d-c9m4p 1/1 Running 10.4.0.2\n",
		"10.96.0.0/16 via 169.254.0.4 dev breth0 src 169.254.0.2 mtu 1440\n",
		"serviceAccountToken:\n  expirationSeconds: 3607\n",
		"users:\n- name: admin\n  user:\n    token: t\n",
		"tokenRequest: enabled\n",
		"Kubernetes config: {BootstrapKubeconfig: CACertData: Token: TokenFile:/var/run/secrets/kubernetes.io/serviceaccount/token CertDir:}\n",
		"[plugins.\"io.containerd.grpc.v1.cri\".registry.configs.\"registry\".auth]\n  username = \"nvke\"\n",
		"capabilities = [\"pull\", \"resolve\"]\n",
		"oauth: enabled12\nauthentication: webhook\nauth: none\n",
		"a basic configuration with basic authentication\n",
		"valueFrom:\n  secretKeyRef:\n    name: db\n    key: password\n",
	} {
		if out := string(scrubBytes([]byte(keep), patterns)); out != keep {
			t.Errorf("harmless text was changed:\nbefore: %q\nafter:  %q", keep, out)
		}
	}
}

func TestRedactBundleRemovesKnownValuesAndShapes(t *testing.T) {
	// The registry password is a known value; its Basic form is not, and the
	// shape has to catch it.
	basic := base64.StdEncoding.EncodeToString([]byte("nvke:s3cretpassw0rd"))
	archive := tarGz(t, map[string]string{
		"site-bundle/node/journal/units/kubelet.log": "the key is FORGEKEY0123456789 and the token is abcdef.0123456789abcdef\n",
		"site-bundle/cluster/configmaps.yaml":        "data:\n  password: s3cretpassw0rd\n",
		"site-bundle/node/config/containerd-certs.d.txt": "== /etc/containerd/certs.d/registry/hosts.toml\n[host.\"https://registry\".header]\n" +
			"  Authorization = [\"Basic " + basic + "\"]\n",
	})
	out, findings, err := redactBundle(archive, []string{"FORGEKEY0123456789", "short", "s3cretpassw0rd"})
	if err != nil {
		t.Fatalf("redactBundle: %v", err)
	}
	if len(findings) > 0 {
		t.Fatalf("findings on a clean archive: %v", findings)
	}
	members := readArchive(t, out)
	for member, gone := range map[string]string{
		"site-bundle/node/journal/units/kubelet.log":     "FORGEKEY0123456789",
		"site-bundle/cluster/configmaps.yaml":            "s3cretpassw0rd",
		"site-bundle/node/config/containerd-certs.d.txt": basic,
	} {
		if strings.Contains(members[member], gone) {
			t.Errorf("%s still carries %q:\n%s", member, gone, members[member])
		}
	}
	if !strings.Contains(members["site-bundle/node/journal/units/kubelet.log"], redactedMarker) {
		t.Errorf("the known value's replacement is not marked:\n%s", members["site-bundle/node/journal/units/kubelet.log"])
	}
}

// A member the collector must never produce is a finding whatever it holds.
func TestRedactBundleRejectsDeniedMembers(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"site-bundle/node/pki/ca.key":             "nothing",
		"site-bundle/node/config/site.kubeconfig": "nothing",
		"site-bundle/node/etc/nke/spire.env":      "nothing",
		"site-bundle/node/values/postgres.yaml":   "nothing",
		"site-bundle/node/cni/net.d/10-ovn.conf":  "fine",
	})
	_, findings, err := redactBundle(archive, nil)
	if err != nil {
		t.Fatalf("redactBundle: %v", err)
	}
	if len(findings) != 4 {
		t.Fatalf("findings = %v, want the four denied members", findings)
	}
	for _, f := range findings {
		if f.what != "denied member name" || strings.HasSuffix(f.member, "10-ovn.conf") {
			t.Errorf("unexpected finding: %v", f)
		}
	}
	if note := describeFindings(findings); !strings.Contains(note, "site-bundle/node/pki/ca.key: denied member name") {
		t.Errorf("the note does not name the member:\n%s", note)
	}
}
