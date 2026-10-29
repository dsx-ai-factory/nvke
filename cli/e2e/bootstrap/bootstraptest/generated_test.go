// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func postgresSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: postgresNamespace, Name: postgresSecretName},
		Data:       data,
	}
}

func TestPostgresPassword(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		failGet bool
		want    string
		wantErr error
	}{
		{name: "the password", objects: []runtime.Object{postgresSecret(map[string][]byte{"password": []byte("pg-secret-1")})}, want: "pg-secret-1"},
		{name: "no Secret yet", wantErr: ErrNotGenerated},
		{name: "a Secret with no password", objects: []runtime.Object{postgresSecret(map[string][]byte{"other": []byte("x")})}, wantErr: ErrUnreadable},
		{name: "an empty password", objects: []runtime.Object{postgresSecret(map[string][]byte{"password": {}})}, wantErr: ErrUnreadable},
		{name: "the API fails with anything but NotFound", failGet: true, wantErr: ErrUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kube := fake.NewClientset(tt.objects...)
			if tt.failGet {
				kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("connection refused")
				})
			}
			got, err := PostgresPassword(context.Background(), kube)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func keyManifest(active string) string {
	return `{"apiVersion":"v1","kind":"Secret","data":{"active":"` + active + `","active-since":"MQ=="}}` + "\n"
}

func TestEtcdKeyValues(t *testing.T) {
	text := "a printable key, 32 chars long!!"
	binary := string([]byte{0x01, 0x02, 0xff, 0xfe, 0x80, 0x90, 0xa0, 0xb0, 0xc0, 0xd0, 0xe0, 0xf0, 0x11, 0x12, 0x13, 0x14})

	tests := []struct {
		name    string
		content *string
		want    []string
		wantErr error
	}{
		{name: "a key that is text", content: ptr(keyManifest(b64(text))), want: []string{b64(text), text}},
		{name: "a key of random bytes", content: ptr(keyManifest(b64(binary))), want: []string{b64(binary)}},
		{
			name: "a key with a NUL byte, which no output prints as text", content: ptr(keyManifest(b64("a printable key\x00, 32 chars long!!"))),
			want: []string{b64("a printable key\x00, 32 chars long!!")},
		},
		{name: "no file yet", wantErr: ErrNotGenerated},
		{name: "a file with no active key", content: ptr(`{"data":{"other":"x"}}`), wantErr: ErrUnreadable},
		{name: "a file that is not a manifest", content: ptr("not json"), wantErr: ErrUnreadable},
		{name: "an empty file", content: ptr(""), wantErr: ErrUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key.json")
			if tt.content != nil {
				require.NoError(t, os.WriteFile(path, []byte(*tt.content), 0o600))
			}
			got, err := EtcdKeyValues(path)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func ptr[T any](v T) *T { return &v }

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func controllerDeployment(env ...corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-controller", Name: "nvke-controller"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "sidecar"}, {Name: "controller", Env: env}},
		}}},
	}
}

func TestVerifyControllerOTLPHeaders(t *testing.T) {
	const want = "authorization=Bearer%20" + planted + ",x-tenant=" + planted

	tests := []struct {
		name    string
		objects []runtime.Object
		wantErr bool
	}{
		{name: "the headers", objects: []runtime.Object{controllerDeployment(corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: want})}},
		{
			name: "other headers",
			objects: []runtime.Object{controllerDeployment(
				corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: "authorization=Bearer%20other"},
			)},
			wantErr: true,
		},
		{name: "no such variable", objects: []runtime.Object{controllerDeployment(corev1.EnvVar{Name: "OTHER", Value: want})}, wantErr: true},
		{
			name: "the variable from a Secret, which has no value",
			objects: []runtime.Object{controllerDeployment(corev1.EnvVar{
				Name:      "OTEL_EXPORTER_OTLP_HEADERS",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{}},
			})},
			wantErr: true,
		},
		{name: "no Deployment", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyControllerOTLPHeaders(context.Background(), fake.NewClientset(tt.objects...), "nvke-controller", "nvke-controller", want)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.NotContains(t, err.Error(), planted, "the error goes to a public CI log")
		})
	}
}

func TestKindClusterConnect(t *testing.T) {
	// kubeconfigFor is what kind returns for a cluster whose API server is at url.
	kubeconfigFor := func(url string) []byte {
		return []byte(`apiVersion: v1
kind: Config
clusters:
- name: kind-x
  cluster:
    server: ` + url + `
contexts:
- name: kind-x
  context: {cluster: kind-x, user: kind-x}
current-context: kind-x
users:
- name: kind-x
  user: {token: not-a-real-token}
`)
	}
	apiServer := func(t *testing.T, status int) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/version" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"major":"1","minor":"32","gitVersion":"v1.32.0"}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	run := func(kubeconfig []byte) func(context.Context, ...string) ([]byte, error) {
		return func(context.Context, ...string) ([]byte, error) { return kubeconfig, nil }
	}

	t.Run("a cluster whose API server answers", func(t *testing.T) {
		srv := apiServer(t, http.StatusOK)
		var got []string
		cluster := KindCluster{Name: "nvkectl-bootstrap", Run: func(_ context.Context, args ...string) ([]byte, error) {
			got = args
			return kubeconfigFor(srv.URL), nil
		}}
		kube, err := cluster.Connect(context.Background())
		require.NoError(t, err)
		assert.NotNil(t, kube)
		assert.Equal(t, []string{"get", "kubeconfig", "--name", "nvkectl-bootstrap"}, got)
	})
	t.Run("kind returns a kubeconfig before the API server is up", func(t *testing.T) {
		srv := apiServer(t, http.StatusOK)
		url := srv.URL
		srv.Close() // nothing is listening: the node exists and the API server does not answer yet
		kube, err := KindCluster{Name: "x", Run: run(kubeconfigFor(url))}.Connect(context.Background())
		require.Error(t, err, "a kubeconfig alone does not mean the cluster answered")
		assert.Nil(t, kube)
		assert.Contains(t, err.Error(), "did not answer")
	})
	t.Run("the API server answers with an error", func(t *testing.T) {
		srv := apiServer(t, http.StatusServiceUnavailable)
		_, err := KindCluster{Name: "x", Run: run(kubeconfigFor(srv.URL))}.Connect(context.Background())
		assert.Error(t, err)
	})
	t.Run("kind fails", func(t *testing.T) {
		cluster := KindCluster{Name: "nvkectl-bootstrap", Run: func(context.Context, ...string) ([]byte, error) {
			return nil, errors.New("no cluster")
		}}
		kube, err := cluster.Connect(context.Background())
		require.Error(t, err)
		assert.Nil(t, kube)
	})
	t.Run("kind returns something that is not a kubeconfig", func(t *testing.T) {
		_, err := KindCluster{Name: "x", Run: run([]byte("not yaml: ["))}.Connect(context.Background())
		assert.Error(t, err)
	})
}
