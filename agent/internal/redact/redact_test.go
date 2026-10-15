// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The credentials below are built from repeated characters, not written out, so
// that no test file holds something that looks like a real token or key. They
// have the shapes kubeadm checks for.
var (
	tokenID        = "aaaaaa"
	tokenSecret    = strings.Repeat("b", 16)
	token          = tokenID + "." + tokenSecret
	certificateKey = strings.Repeat("c", 64)
	caCertHash     = "sha256:" + strings.Repeat("d", 64)
	redactedToken  = tokenID + ".<redacted>"
)

func TestStringRedactsByContext(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "join command flags",
			in: fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s --control-plane --certificate-key %s",
				token, caCertHash, certificateKey),
			want: fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s --control-plane --certificate-key <redacted>",
				redactedToken, caCertHash),
		},
		{
			name: "the other token flags and the --flag=value form, but not --token-ttl",
			in:   fmt.Sprintf("join --discovery-token %s --tls-bootstrap-token=%s --token-ttl 24h --certificate-key=%s", token, token, certificateKey),
			want: fmt.Sprintf("join --discovery-token %[1]s --tls-bootstrap-token=%[1]s --token-ttl 24h --certificate-key=<redacted>", redactedToken),
		},
		{
			name: "the lines kubeadm prints its generated credentials on",
			in:   fmt.Sprintf("[bootstrap-token] Using token: %s\n[upload-certs] Using certificate key:\n%s\n", token, certificateKey),
			want: fmt.Sprintf("[bootstrap-token] Using token: %s\n[upload-certs] Using certificate key:\n<redacted>\n", redactedToken),
		},
		{
			name: "prose that names a flag is left alone",
			in:   "error: flag --token is required\nunknown flag: --certificate-key\nSee 'kubeadm join --help'",
			want: "error: flag --token is required\nunknown flag: --certificate-key\nSee 'kubeadm join --help'",
		},
		{
			name: "token-shaped text with no credential context is left alone",
			in:   "dial tcp: lookup node01.abcdefghijklmnop.example: no such host; " + token,
			want: "dial tcp: lookup node01.abcdefghijklmnop.example: no such host; " + token,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := New().String(tc.in)

			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, New().String(got), "redacting twice changes nothing")
		})
	}
}

func TestStringRedactsKnownValues(t *testing.T) {
	credentials := New()
	credentials.AddJoinString(fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --certificate-key %s", token, certificateKey))
	credentials.AddSecret("short")

	got := credentials.String(fmt.Sprintf("token %s is invalid, key %s, node01.abcdefghijklmnop.example, a short value", token, certificateKey))

	assert.Equal(t, fmt.Sprintf("token %s is invalid, key <redacted>, node01.abcdefghijklmnop.example, a short value", redactedToken), got)

	t.Run("a value given to the agent is replaced whatever its shape", func(t *testing.T) {
		given := New()
		given.AddJoinString("kubeadm join 10.0.0.1:6443 --token not-a-token-shape")

		assert.Equal(t, "kubeadm join 10.0.0.1:6443 --token <redacted>", given.String("kubeadm join 10.0.0.1:6443 --token not-a-token-shape"))
	})
}

func TestAddConfig(t *testing.T) {
	cases := []struct {
		name   string
		config string
		secret string // the whole credential: a token or a certificate key
		kept   []string
	}{
		{
			name:   "block style, with a comment",
			config: fmt.Sprintf("kind: JoinConfiguration\ndiscovery:\n  bootstrapToken:\n    token: %s # the join token\n  timeout: 5m0s\n", token),
			secret: token,
			kept:   []string{"# the join token", "timeout: 5m0s"},
		},
		{
			name:   "inline mapping",
			config: fmt.Sprintf("kind: JoinConfiguration\ndiscovery: {bootstrapToken: {token: %s, apiServerEndpoint: 10.0.0.1:6443}}\n", token),
			secret: token,
			kept:   []string{"apiServerEndpoint: 10.0.0.1:6443"},
		},
		{
			name:   "quoted, in a list item",
			config: fmt.Sprintf("kind: InitConfiguration\nbootstrapTokens:\n- token: '%s'\n  ttl: 24h0m0s\n", token),
			secret: token,
			kept:   []string{"ttl: 24h0m0s"},
		},
		{
			name:   "the certificate key, in the second document",
			config: fmt.Sprintf("kind: ClusterConfiguration\n---\nkind: JoinConfiguration\ncontrolPlane:\n  certificateKey: \"%s\"\n", certificateKey),
			secret: certificateKey,
			kept:   []string{"kind: ClusterConfiguration"},
		},
		{
			name:   "documents after a separator with a comment, and Windows line endings",
			config: fmt.Sprintf("kind: ClusterConfiguration\r\n--- # the join configuration\r\nkind: JoinConfiguration\r\ndiscovery:\r\n  tlsBootstrapToken: %s\r\n", token),
			secret: token,
			kept:   []string{"kind: ClusterConfiguration", "--- # the join configuration"},
		},
		{
			name:   "a value written with an escape sequence",
			config: fmt.Sprintf("kind: JoinConfiguration\ndiscovery:\n  tlsBootstrapToken: \"%s\\u0061.%s\"\n", tokenID[:5], tokenSecret),
			secret: tokenID[:5] + `\u0061.` + tokenSecret,
			kept:   []string{"kind: JoinConfiguration"},
		},
		{
			name:   "a document that does not parse is searched line by line",
			config: fmt.Sprintf("kind: JoinConfiguration\n\tbad: [\ncertificateKey: %s\n", certificateKey),
			secret: certificateKey,
			kept:   []string{"\tbad: ["},
		},
		{
			name:   "flow style, keeping the token ID, in a document that does not parse",
			config: fmt.Sprintf("kind: JoinConfiguration\n\tbad: [\ndiscovery: {bootstrapToken: {token: %s, apiServerEndpoint: 10.0.0.1:6443}}\n", token),
			secret: token,
			kept:   []string{"\tbad: [", "apiServerEndpoint: 10.0.0.1:6443}}"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credentials := New()
			credentials.AddConfig(tc.config)

			got := credentials.String(tc.config)

			replacement := Redacted
			if strings.HasSuffix(tc.secret, tokenSecret) {
				replacement = redactedToken
			}
			assert.NotContains(t, got, tc.secret)
			assert.Equal(t, strings.Replace(tc.config, tc.secret, replacement, 1), got, "only the credential changed")
			for _, kept := range tc.kept {
				assert.Contains(t, got, kept)
			}
		})
	}

	t.Run("an empty credential does not take the next field with it", func(t *testing.T) {
		const config = "bootstrapTokens:\n- token:\n  ttl: 24h0m0s\n"
		credentials := New()
		credentials.AddConfig(config)

		assert.Equal(t, config, credentials.String(config))
	})
}

func TestArgs(t *testing.T) {
	args := []string{"join", "--token", token, "--certificate-key=" + certificateKey, "--discovery-token-ca-cert-hash", caCertHash, "--token-ttl", "24h"}
	original := append([]string(nil), args...)

	got := Args(args)

	assert.Equal(t, []string{"join", "--token", redactedToken, "--certificate-key=<redacted>", "--discovery-token-ca-cert-hash", caCertHash, "--token-ttl", "24h"}, got)
	assert.Equal(t, original, args, "the input must not be modified")
	assert.Equal(t, []string{"join", "--token"}, Args([]string{"join", "--token"}), "a flag with no value is left alone")
	assert.Empty(t, Args(nil))
}
