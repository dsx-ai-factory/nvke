// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Ported from the site-workflow-engine cluster controller's
// helpers_internal_test.go. The golden files under testdata/ are byte copies of
// SWE's, so a passing TestBuildSpireKubeconfig proves this controller emits the
// same credential SWE does.

package tenantaccess

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
)

// update regenerates the golden files instead of asserting against them:
// `go test ./internal/controllers/tenantaccess/... -run TestBuildSpireKubeconfig -update`.
var update = flag.Bool("update", false, "regenerate golden files")

const testEndpointSocket = "unix:///var/run/spire/agent/sockets/public/api.sock"

func TestBuildSpireKubeconfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		golden      string
		clusterName string
		server      string
		ca          []byte
		exchange    string
	}{
		{name: "with CA", golden: "spire_kubeconfig_with_ca.yaml", clusterName: "alpha", server: "https://1.2.3.4:6443", ca: []byte(testCAPEM)},
		{name: "empty CA", golden: "spire_kubeconfig_empty_ca.yaml", clusterName: "beta", server: "https://5.6.7.8:6443", ca: nil},
		// The exchange case carries one extra env var and nothing else: the audience
		// and the socket are unchanged, so turning the exchange on at a site does not
		// change what the tenant cluster is asked to trust beyond the issuer of the
		// token it receives.
		{name: "with exchange", golden: "spire_kubeconfig_with_exchange.yaml", clusterName: "gamma", server: "https://9.10.11.12:6443", ca: []byte(testCAPEM), exchange: "https://exchange.example/token"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := buildSpireKubeconfig(tc.clusterName, tc.server, tc.ca, testEndpointSocket, tc.exchange)
			require.NoError(t, err)

			assertGolden(t, tc.golden, out)
		})
	}
}

// TestReadAndApplyRBACManifest exercises readRBACManifest and applyManifestSSA
// together: the manifest is seeded into the nke-rbac ConfigMap, read back, and
// then server-side-applied — covering both helpers over one client.
func TestReadAndApplyRBACManifest(t *testing.T) {
	t.Parallel()

	const singleDoc = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: nvke-role\nrules: []\n"
	const multiDoc = singleDoc + "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: nvke-binding\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: nvke-role\nsubjects: []\n"

	wantRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "nvke-role"},
		Rules:      []rbacv1.PolicyRule{},
	}
	wantBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "nvke-binding"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "nvke-role"},
		// SSA drops the manifest's empty "subjects: []" to nil on round-trip.
		Subjects: nil,
	}

	tests := []struct {
		name     string
		manifest string
		wantErr  string
		want     []ctrlclient.Object
	}{
		{name: "single doc", manifest: singleDoc, want: []ctrlclient.Object{wantRole}},
		{name: "multi doc", manifest: multiDoc, want: []ctrlclient.Object{wantRole, wantBinding}},
		{name: "empty manifest", manifest: "", wantErr: "is empty"},
		{name: "malformed yaml", manifest: "\t- this is not: valid: yaml: at all", wantErr: "decoding RBAC manifest"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
				Data:       map[string]string{rbacConfigMapKey: tc.manifest},
			}
			c := crfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm).Build()
			ctrl := &Controller{k8sClient: c}

			manifest, err := ctrl.readRBACManifest(ctx)
			if err == nil {
				err = applyManifestSSA(ctx, c, manifest, rbacFieldManager)
			}
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			assertApplied(t, c, tc.want)

			// Re-apply is a no-op (idempotent): the applied objects are unchanged.
			require.NoError(t, applyManifestSSA(ctx, c, manifest, rbacFieldManager))
			assertApplied(t, c, tc.want)
		})
	}
}

// TestReadRBACManifestMissingKey covers the branch the manifest table cannot:
// the ConfigMap exists but carries no manifest.yaml key at all.
func TestReadRBACManifestMissingKey(t *testing.T) {
	t.Parallel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
		Data:       map[string]string{"other.yaml": "irrelevant"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm).Build()
	ctrl := &Controller{k8sClient: c}

	_, err := ctrl.readRBACManifest(context.Background())
	require.ErrorContains(t, err, "missing required key")
}

// TestReadRBACManifestAbsentConfigMap covers an unbootstrapped namespace: the
// operator-managed ConfigMap has not been created yet.
func TestReadRBACManifestAbsentConfigMap(t *testing.T) {
	t.Parallel()

	c := crfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	ctrl := &Controller{k8sClient: c}

	_, err := ctrl.readRBACManifest(context.Background())
	require.ErrorContains(t, err, "getting NVKE RBAC ConfigMap")
}

func assertApplied(t *testing.T, c ctrlclient.Client, want []ctrlclient.Object) {
	t.Helper()

	for _, w := range want {
		got, ok := reflect.New(reflect.TypeOf(w).Elem()).Interface().(ctrlclient.Object)
		require.True(t, ok)
		require.NoError(t, c.Get(context.Background(), ctrlclient.ObjectKeyFromObject(w), got))

		got.SetResourceVersion("")
		got.SetManagedFields(nil)
		assert.Equal(t, w, got)
	}
}

// assertGolden compares got against the named file under testdata, or rewrites
// that file when -update is set.
func assertGolden(t *testing.T, goldenFile string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", goldenFile)
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o600))
		return
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err, "reading golden file %q (run with -update to create)", goldenFile)
	assert.Equal(t, string(want), string(got), "golden mismatch for %s", goldenFile)
}

func legacyClusterObjects() []ctrlclient.Object {
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }
	nsMeta := func(ns string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: "nke:cwe", Namespace: ns} }
	return []ctrlclient.Object{
		&rbacv1.ClusterRole{ObjectMeta: meta("nke:cwe")},
		&rbacv1.ClusterRoleBinding{ObjectMeta: meta("nke:cwe")},
		&rbacv1.ClusterRoleBinding{ObjectMeta: meta("nke:capi-cluster-admin")},
		&rbacv1.Role{ObjectMeta: nsMeta("kube-system")},
		&rbacv1.RoleBinding{ObjectMeta: nsMeta("kube-system")},
		&rbacv1.Role{ObjectMeta: nsMeta("kube-node-lease")},
		&rbacv1.RoleBinding{ObjectMeta: nsMeta("kube-node-lease")},
	}
}

func TestPruneLegacyRBAC(t *testing.T) {
	t.Parallel()

	const renamed = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: nvke:cwe\nrules: []\n"
	const pinned = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: nke:cwe\nrules: []\n"

	t.Run("removes every legacy object and keeps the renamed ones", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		keep := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "nvke:cwe"}}
		c := crfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(append(legacyClusterObjects(), keep)...).Build()

		require.NoError(t, pruneLegacyRBAC(ctx, c, []byte(renamed)))
		for _, obj := range legacyClusterObjects() {
			err := c.Get(ctx, ctrlclient.ObjectKeyFromObject(obj), obj)
			require.True(t, apierrors.IsNotFound(err), "%T %s should be gone", obj, obj.GetName())
		}
		require.NoError(t, c.Get(ctx, ctrlclient.ObjectKeyFromObject(keep), &rbacv1.ClusterRole{}))

		// Nothing left to delete is not an error.
		require.NoError(t, pruneLegacyRBAC(ctx, c, []byte(renamed)))
	})

	t.Run("keeps a legacy name the manifest still carries", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		c := crfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(legacyClusterObjects()...).Build()

		require.NoError(t, pruneLegacyRBAC(ctx, c, []byte(pinned)))
		require.NoError(t, c.Get(ctx, ctrlclient.ObjectKey{Name: "nke:cwe"}, &rbacv1.ClusterRole{}))
		err := c.Get(ctx, ctrlclient.ObjectKey{Name: "nke:capi-cluster-admin"}, &rbacv1.ClusterRoleBinding{})
		require.True(t, apierrors.IsNotFound(err))
	})
}
