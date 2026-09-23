// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

// baselinePatchManifest matches real kubeadm output: all flags live under
// container.command, with container.args absent.
const baselinePatchManifest = `apiVersion: v1
kind: Pod
metadata:
  name: kube-apiserver
  namespace: kube-system
spec:
  containers:
  - name: kube-apiserver
    image: registry.k8s.io/kube-apiserver:v1.30.0
    command:
    - kube-apiserver
    - --advertise-address=10.0.0.1
    - --allow-privileged=true
    volumeMounts:
    - mountPath: /etc/kubernetes/pki
      name: k8s-certs
      readOnly: true
  volumes:
  - hostPath:
      path: /etc/kubernetes/pki
      type: DirectoryOrCreate
    name: k8s-certs
`

// ── ensureFlag ───────────────────────────────────────────────────────────────

func TestEnsureFlag(t *testing.T) {
	tests := []struct {
		name  string
		cmd   []string
		flag  string
		value string
		want  []string
	}{
		{
			name:  "add when absent",
			cmd:   []string{"kube-apiserver", "--allow-privileged=true"},
			flag:  "authentication-config",
			value: "/etc/kubernetes/oidc/auth.yaml",
			want:  []string{"kube-apiserver", "--allow-privileged=true", "--authentication-config=/etc/kubernetes/oidc/auth.yaml"},
		},
		{
			name:  "no-op when identical",
			cmd:   []string{"kube-apiserver", "--authentication-config=/x"},
			flag:  "authentication-config",
			value: "/x",
			want:  []string{"kube-apiserver", "--authentication-config=/x"},
		},
		{
			name:  "replace when value differs (token form)",
			cmd:   []string{"kube-apiserver", "--authentication-config=/old"},
			flag:  "authentication-config",
			value: "/new",
			want:  []string{"kube-apiserver", "--authentication-config=/new"},
		},
		{
			name:  "split form sets following value token",
			cmd:   []string{"kube-apiserver", "--authentication-config", "/old"},
			flag:  "authentication-config",
			value: "/new",
			want:  []string{"kube-apiserver", "--authentication-config", "/new"},
		},
		{
			name:  "bare boolean flag elsewhere is untouched",
			cmd:   []string{"kube-apiserver", "--profiling", "--allow-privileged=true"},
			flag:  "authentication-config",
			value: "/x",
			want:  []string{"kube-apiserver", "--profiling", "--allow-privileged=true", "--authentication-config=/x"},
		},
		{
			// Setting a bare boolean flag that is followed by another flag must
			// normalize the bare flag to token form, NOT overwrite the next flag.
			name:  "set bare boolean flag followed by another flag",
			cmd:   []string{"kube-apiserver", "--profiling", "--allow-privileged=true"},
			flag:  "profiling",
			value: "false",
			want:  []string{"kube-apiserver", "--profiling=false", "--allow-privileged=true"},
		},
		{
			name:  "set bare boolean flag at end normalizes to token form",
			cmd:   []string{"kube-apiserver", "--allow-privileged=true", "--profiling"},
			flag:  "profiling",
			value: "false",
			want:  []string{"kube-apiserver", "--allow-privileged=true", "--profiling=false"},
		},
		{
			name:  "idempotent re-run after add",
			cmd:   []string{"kube-apiserver", "--authentication-config=/x"},
			flag:  "authentication-config",
			value: "/x",
			want:  []string{"kube-apiserver", "--authentication-config=/x"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ensureFlag(tc.cmd, tc.flag, tc.value)
			assert.Equal(t, tc.want, got)
			// Idempotency: applying again yields the same result.
			assert.Equal(t, tc.want, ensureFlag(got, tc.flag, tc.value))
		})
	}
}

// ── removeFlag ───────────────────────────────────────────────────────────────

func TestRemoveFlag(t *testing.T) {
	tests := []struct {
		name string
		cmd  []string
		flag string
		want []string
	}{
		{
			name: "token form",
			cmd:  []string{"kube-apiserver", "--authentication-config=/x", "--allow-privileged=true"},
			flag: "authentication-config",
			want: []string{"kube-apiserver", "--allow-privileged=true"},
		},
		{
			name: "split form drops flag and value token",
			cmd:  []string{"kube-apiserver", "--authentication-config", "/x", "--allow-privileged=true"},
			flag: "authentication-config",
			want: []string{"kube-apiserver", "--allow-privileged=true"},
		},
		{
			name: "absent is a no-op",
			cmd:  []string{"kube-apiserver", "--allow-privileged=true"},
			flag: "authentication-config",
			want: []string{"kube-apiserver", "--allow-privileged=true"},
		},
		{
			name: "multiple occurrences all removed",
			cmd:  []string{"kube-apiserver", "--foo=1", "--foo=2", "--foo", "3", "--bar=9"},
			flag: "foo",
			want: []string{"kube-apiserver", "--bar=9"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := removeFlag(tc.cmd, tc.flag)
			assert.Equal(t, tc.want, got)
			// Idempotency.
			assert.Equal(t, tc.want, removeFlag(got, tc.flag))
		})
	}
}

// ── applyFlagOps ─────────────────────────────────────────────────────────────

func apiserverCommand(t *testing.T, manifestJSON []byte) []string {
	t.Helper()
	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(manifestJSON, &doc))
	container, err := findAPIServerContainerMap(doc)
	require.NoError(t, err)
	cmd, err := toStringSlice(container["command"])
	require.NoError(t, err)
	return cmd
}

func TestApplyFlagOps_MultiOpOrdering(t *testing.T) {
	manifest := []byte(`{"apiVersion":"v1","kind":"Pod","spec":{"containers":[{"name":"kube-apiserver","command":["kube-apiserver","--foo=1"]}]}}`)
	ops := []*pb.FlagOp{
		{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "foo", Value: "2"}}},
		{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "bar", Value: "3"}}},
	}
	out, err := applyFlagOps(manifest, ops)
	require.NoError(t, err)
	assert.Equal(t, []string{"kube-apiserver", "--foo=2", "--bar=3"}, apiserverCommand(t, out))
}

func TestApplyFlagOps_SetThenRemove(t *testing.T) {
	manifest := []byte(`{"apiVersion":"v1","kind":"Pod","spec":{"containers":[{"name":"kube-apiserver","command":["kube-apiserver"]}]}}`)
	ops := []*pb.FlagOp{
		{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "baz", Value: "1"}}},
		{Op: &pb.FlagOp_Remove{Remove: &pb.RemoveFlag{Name: "baz"}}},
	}
	out, err := applyFlagOps(manifest, ops)
	require.NoError(t, err)
	assert.Equal(t, []string{"kube-apiserver"}, apiserverCommand(t, out))
}

func TestApplyFlagOps_PrefersExistingLocationInArgs(t *testing.T) {
	// The flag already lives in args (split-shape manifest); ensure the Set
	// reconciles it there rather than duplicating into command.
	manifest := []byte(`{"apiVersion":"v1","kind":"Pod","spec":{"containers":[{"name":"kube-apiserver","command":["kube-apiserver"],"args":["--foo=1"]}]}}`)
	ops := []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "foo", Value: "2"}}}}
	out, err := applyFlagOps(manifest, ops)
	require.NoError(t, err)

	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &doc))
	container, err := findAPIServerContainerMap(doc)
	require.NoError(t, err)
	args, err := toStringSlice(container["args"])
	require.NoError(t, err)
	assert.Equal(t, []string{"--foo=2"}, args)
	assert.Equal(t, []string{"kube-apiserver"}, apiserverCommand(t, out))
}

func TestApplyFlagOps_PreservesUnknownFields(t *testing.T) {
	// A field the vendored corev1 types do not model must survive the map
	// round-trip untouched.
	manifest := []byte(`{"apiVersion":"v1","kind":"Pod","spec":{"customUnknownField":"keepme","containers":[{"name":"kube-apiserver","command":["kube-apiserver"]}]}}`)
	ops := []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "foo", Value: "1"}}}}
	out, err := applyFlagOps(manifest, ops)
	require.NoError(t, err)

	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &doc))
	spec := doc["spec"].(map[string]interface{})
	assert.Equal(t, "keepme", spec["customUnknownField"])
}

// ── strategic merge patch behaviors ──────────────────────────────────────────

func smpMerge(t *testing.T, manifestYAML, patchYAML string) []byte {
	t.Helper()
	origJSON, err := yaml.YAMLToJSON([]byte(manifestYAML))
	require.NoError(t, err)
	patchJSON, err := yaml.YAMLToJSON([]byte(patchYAML))
	require.NoError(t, err)
	out, err := strategicMergeForTest(origJSON, patchJSON)
	require.NoError(t, err)
	return out
}

func TestSMP_AddVolumeAndMountIdempotent(t *testing.T) {
	patch := `spec:
  volumes:
  - name: oidc-auth-config
    hostPath:
      path: /etc/kubernetes/oidc
      type: DirectoryOrCreate
  containers:
  - name: kube-apiserver
    volumeMounts:
    - name: oidc-auth-config
      mountPath: /etc/kubernetes/oidc
      readOnly: true
`
	merged1 := smpMerge(t, baselinePatchManifest, patch)
	assert.Contains(t, string(merged1), "oidc-auth-config")

	// Re-applying the same keyed patch is a no-op (merge keys reconcile).
	origJSON, err := yaml.YAMLToJSON([]byte(patch))
	require.NoError(t, err)
	merged2, err := strategicMergeForTest(merged1, origJSON)
	require.NoError(t, err)
	assert.True(t, jsonSemanticEqual(merged1, merged2), "adding the same volume/mount twice must be idempotent")

	// The pre-existing k8s-certs volume is still present.
	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(merged1, &doc))
	assert.Contains(t, volumeNames(doc), "k8s-certs")
	assert.Contains(t, volumeNames(doc), "oidc-auth-config")
}

func TestSMP_PatchDeleteRemovesVolume(t *testing.T) {
	patch := `spec:
  volumes:
  - name: k8s-certs
    $patch: delete
`
	merged := smpMerge(t, baselinePatchManifest, patch)
	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(merged, &doc))
	assert.NotContains(t, volumeNames(doc), "k8s-certs")
}

// strategicMergeForTest mirrors the handler's strategic-merge call.
func strategicMergeForTest(original, patch []byte) ([]byte, error) {
	return strategicpatch.StrategicMergePatch(original, patch, corev1.Pod{})
}

// ── handler ──────────────────────────────────────────────────────────────────

func TestPatchAPIServerManifest_NotControlPlane(t *testing.T) {
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
	srv.NodeStatus = &NodeStatus{Role: WorkerRole}

	_, err := srv.PatchAPIServerManifest(context.Background(), &pb.PatchAPIServerManifestRequest{
		FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "foo", Value: "1"}}}},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "control-plane node required")
}

func TestPatchAPIServerManifest_RequestValidation(t *testing.T) {
	tests := []struct {
		name string
		req  *pb.PatchAPIServerManifestRequest
		want string
	}{
		{"empty request", &pb.PatchAPIServerManifestRequest{}, "at least one"},
		{
			"non-absolute path",
			&pb.PatchAPIServerManifestRequest{Files: []*pb.File{{Path: "etc/kubernetes/x", Content: []byte("x"), Mode: 0o600, Owner: "root:root"}}},
			"must be absolute",
		},
		{
			// validateKubeadmFilePath (shared with init/join) dedups by cleaned
			// path, so two files resolving to the same path are rejected.
			"duplicate file path",
			&pb.PatchAPIServerManifestRequest{Files: []*pb.File{
				{Path: "/etc/kubernetes/oidc/a.yaml", Content: []byte("x"), Mode: 0o600, Owner: "root:root"},
				{Path: "/etc/kubernetes/oidc/../oidc/a.yaml", Content: []byte("y"), Mode: 0o600, Owner: "root:root"},
			}},
			"duplicate path",
		},
		{
			"flag name with leading dashes",
			&pb.PatchAPIServerManifestRequest{FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "--foo", Value: "1"}}}}},
			"leading dashes",
		},
		{
			"flag name with equals",
			&pb.PatchAPIServerManifestRequest{FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "foo=bar", Value: "1"}}}}},
			"must not contain '='",
		},
		{
			"flag op with neither set nor remove",
			&pb.PatchAPIServerManifestRequest{FlagOps: []*pb.FlagOp{{}}},
			"neither set nor remove",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
			_, err := srv.PatchAPIServerManifest(context.Background(), tc.req)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestPatchAPIServerManifest_Unchanged(t *testing.T) {
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
	seedPatchManifest(t, srv, baselinePatchManifest)

	// Set a flag to a value it already has: a semantic no-op.
	req := &pb.PatchAPIServerManifestRequest{
		FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "allow-privileged", Value: "true"}}}},
	}
	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_UNCHANGED, resp.GetOutcome())
	assert.Empty(t, resp.GetBackupManifestPath())
	assert.Empty(t, listPatchBackups(t, srv), "UNCHANGED must not snapshot the manifest")
}

func TestPatchAPIServerManifest_AppliedFlagAndVolume(t *testing.T) {
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
	seedPatchManifest(t, srv, baselinePatchManifest)
	seedPatchMirrorPodSequence(t, srv, "uid-OLD", "uid-NEW")

	req := &pb.PatchAPIServerManifestRequest{
		StrategicMergePatches: [][]byte{[]byte(`spec:
  volumes:
  - name: oidc-auth-config
    hostPath:
      path: /etc/kubernetes/oidc
      type: DirectoryOrCreate
  containers:
  - name: kube-apiserver
    volumeMounts:
    - name: oidc-auth-config
      mountPath: /etc/kubernetes/oidc
      readOnly: true
`)},
		FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "authentication-config", Value: "/etc/kubernetes/oidc/auth-config.yaml"}}}},
		Files: []*pb.File{{
			Path:    "/etc/kubernetes/oidc/auth-config.yaml",
			Content: []byte("apiVersion: apiserver.config.k8s.io/v1beta1\nkind: AuthenticationConfiguration\n"),
			Mode:    0o600,
			Owner:   "root:root",
		}},
	}
	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_APPLIED, resp.GetOutcome())
	assert.NotEmpty(t, resp.GetBackupManifestPath())

	// Manifest carries the flag and the volume.
	manifestPath := filepath.Join(srv.RootPath, kubeAPIServerManifestPath)
	raw, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	cmdJSON, err := yaml.YAMLToJSON(raw)
	require.NoError(t, err)
	assert.Contains(t, apiserverCommand(t, cmdJSON), "--authentication-config=/etc/kubernetes/oidc/auth-config.yaml")
	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(cmdJSON, &doc))
	assert.Contains(t, volumeNames(doc), "oidc-auth-config")

	// The staged file exists.
	staged, err := os.ReadFile(filepath.Join(srv.RootPath, "/etc/kubernetes/oidc/auth-config.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(staged), "AuthenticationConfiguration")
}

func TestPatchAPIServerManifest_FilesOnlyChangeUsesBareReadiness(t *testing.T) {
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
	seedPatchManifest(t, srv, baselinePatchManifest)
	srv.apiServerReadyMaxAttempts = 2
	srv.apiServerReadyPollInterval = 5 * time.Millisecond

	req := &pb.PatchAPIServerManifestRequest{
		Files: []*pb.File{{
			Path:    "/etc/kubernetes/extra/data.txt",
			Content: []byte("hello"),
			Mode:    0o600,
			Owner:   "root:root",
		}},
	}
	manifestPath := filepath.Join(srv.RootPath, kubeAPIServerManifestPath)
	before, err := os.ReadFile(manifestPath)
	require.NoError(t, err)

	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_APPLIED, resp.GetOutcome())
	assert.Empty(t, resp.GetBackupManifestPath(), "a files-only change never snapshots the manifest")
	assert.Empty(t, listPatchBackups(t, srv))

	after, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "manifest is untouched on a files-only change")

	staged, err := os.ReadFile(filepath.Join(srv.RootPath, "/etc/kubernetes/extra/data.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(staged))
}

func TestPatchAPIServerManifest_MultipleStrategicMergePatches(t *testing.T) {
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
	seedPatchManifest(t, srv, baselinePatchManifest)
	seedPatchMirrorPodSequence(t, srv, "uid-OLD", "uid-NEW")

	// Two separate patches applied in order; both must land. An empty entry in
	// the list is ignored.
	req := &pb.PatchAPIServerManifestRequest{
		StrategicMergePatches: [][]byte{
			[]byte(`spec:
  volumes:
  - name: vol-a
    hostPath:
      path: /etc/kubernetes/a
      type: DirectoryOrCreate
`),
			nil,
			[]byte(`spec:
  volumes:
  - name: vol-b
    hostPath:
      path: /etc/kubernetes/b
      type: DirectoryOrCreate
`),
		},
	}
	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_APPLIED, resp.GetOutcome())

	manifestPath := filepath.Join(srv.RootPath, kubeAPIServerManifestPath)
	raw, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	mergedJSON, err := yaml.YAMLToJSON(raw)
	require.NoError(t, err)
	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(mergedJSON, &doc))
	names := volumeNames(doc)
	assert.Contains(t, names, "vol-a")
	assert.Contains(t, names, "vol-b")
}

func TestPatchAPIServerManifest_RestoreLaneFailedRestored(t *testing.T) {
	// Forward mutation lands but the apiserver never returns Ready, so the
	// restore runs, rewrites the original bytes, and the apiserver comes back.
	// The node is confirmed restored, so FAILED_RESTORED.
	seq := newReadinessSequence(
		http.StatusServiceUnavailable, // forward probe 1
		http.StatusServiceUnavailable, // forward probe 2
		http.StatusServiceUnavailable, // forward probe 3 -> forward fails
		http.StatusOK,                 // restore -> succeeds
	)
	srv := newPatchTestServer(t, seq.handler())
	seedPatchManifest(t, srv, baselinePatchManifest)
	srv.apiServerReadyMaxAttempts = 2
	srv.apiServerReadyPollInterval = 5 * time.Millisecond

	manifestPath := filepath.Join(srv.RootPath, kubeAPIServerManifestPath)
	before, err := os.ReadFile(manifestPath)
	require.NoError(t, err)

	req := &pb.PatchAPIServerManifestRequest{
		FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "authentication-config", Value: "/etc/kubernetes/oidc/auth-config.yaml"}}}},
	}
	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED, resp.GetOutcome())
	assert.NotEmpty(t, resp.GetBackupManifestPath())

	after, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "manifest restored to pre-call bytes")
}

func TestPatchAPIServerManifest_RestoreLaneFailedNotRestored(t *testing.T) {
	// The apiserver never returns Ready: the forward mutation fails, the restore
	// rewrites the bytes but readiness still cannot be confirmed, so the outcome
	// is FAILED_NOT_RESTORED even though the manifest is byte-restored.
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusServiceUnavailable))
	seedPatchManifest(t, srv, baselinePatchManifest)
	srv.apiServerReadyMaxAttempts = 2
	srv.apiServerReadyPollInterval = 5 * time.Millisecond

	manifestPath := filepath.Join(srv.RootPath, kubeAPIServerManifestPath)
	before, err := os.ReadFile(manifestPath)
	require.NoError(t, err)

	req := &pb.PatchAPIServerManifestRequest{
		FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "authentication-config", Value: "/etc/kubernetes/oidc/auth-config.yaml"}}}},
	}
	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED, resp.GetOutcome())
	assert.Contains(t, resp.GetErrorMessage(), "not confirmed restored")

	after, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "restore still rewrites the original bytes even when readiness is unconfirmed")
	assert.NotEmpty(t, resp.GetBackupManifestPath())
}

func TestPatchAPIServerManifest_OverwriteRollbackRestoresPriorFile(t *testing.T) {
	// A manifest change plus an in-place overwrite of an existing file. The
	// forward mutation lands but readiness fails, so the restore runs. Both the
	// manifest and the overwritten file must return to their pre-call state.
	seq := newReadinessSequence(
		http.StatusServiceUnavailable, // forward probe 1
		http.StatusServiceUnavailable, // forward probe 2
		http.StatusServiceUnavailable, // forward probe 3 -> forward fails
		http.StatusOK,                 // restore -> succeeds
	)
	srv := newPatchTestServer(t, seq.handler())
	seedPatchManifest(t, srv, baselinePatchManifest)
	srv.apiServerReadyMaxAttempts = 2
	srv.apiServerReadyPollInterval = 5 * time.Millisecond

	authPath := filepath.Join(srv.RootPath, "/etc/kubernetes/oidc/auth-config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(authPath), 0o755))
	require.NoError(t, os.WriteFile(authPath, []byte("OLD"), 0o600))

	req := &pb.PatchAPIServerManifestRequest{
		FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "authentication-config", Value: "/etc/kubernetes/oidc/auth-config.yaml"}}}},
		Files: []*pb.File{{
			Path:    "/etc/kubernetes/oidc/auth-config.yaml",
			Content: []byte("NEW"),
			Mode:    0o600,
			Owner:   "root:root",
		}},
	}
	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED, resp.GetOutcome())

	restored, err := os.ReadFile(authPath)
	require.NoError(t, err)
	assert.Equal(t, "OLD", string(restored), "an overwritten file must be restored to its prior content on rollback")
}

func TestPatchAPIServerManifest_FilesOnlyFailureRollsBackAndRestores(t *testing.T) {
	// A files-only change (no manifest edit) to a file the apiserver watches. The
	// staged content makes readiness fail; rolling the file back to its prior
	// content lets the apiserver recover, so the outcome is FAILED_RESTORED — not
	// the FAILED_NOT_RESTORED the old code returned by treating a files-only
	// change as unrecoverable.
	var authPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		// The apiserver is unhealthy while the staged (bad) content is on disk,
		// and healthy once the file is rolled back.
		content, _ := os.ReadFile(authPath)
		if string(content) == "NEW" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := newPatchTestServer(t, handler)
	seedPatchManifest(t, srv, baselinePatchManifest)
	srv.apiServerReadyMaxAttempts = 2
	srv.apiServerReadyPollInterval = 5 * time.Millisecond

	authPath = filepath.Join(srv.RootPath, "/etc/kubernetes/oidc/auth-config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(authPath), 0o755))
	require.NoError(t, os.WriteFile(authPath, []byte("OLD"), 0o600))

	manifestPath := filepath.Join(srv.RootPath, kubeAPIServerManifestPath)
	manifestBefore, err := os.ReadFile(manifestPath)
	require.NoError(t, err)

	req := &pb.PatchAPIServerManifestRequest{
		Files: []*pb.File{{
			Path:    "/etc/kubernetes/oidc/auth-config.yaml",
			Content: []byte("NEW"),
			Mode:    0o600,
			Owner:   "root:root",
		}},
	}
	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED, resp.GetOutcome())
	assert.Empty(t, resp.GetBackupManifestPath(), "a files-only change never snapshots the manifest")
	assert.Empty(t, listPatchBackups(t, srv))

	restored, err := os.ReadFile(authPath)
	require.NoError(t, err)
	assert.Equal(t, "OLD", string(restored), "files-only failure must roll the file back to its prior content")

	manifestAfter, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter, "a files-only change never touches the manifest")
}

func TestPatchAPIServerManifest_FilesNotStagedWhenComputeFails(t *testing.T) {
	// Staging happens only after the in-memory manifest computation succeeds, so a
	// compute failure — here an unparseable strategic-merge patch — stages no file.
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
	seedPatchManifest(t, srv, baselinePatchManifest)

	newFile := filepath.Join(srv.RootPath, "/etc/kubernetes/extra/new.txt")

	req := &pb.PatchAPIServerManifestRequest{
		StrategicMergePatches: [][]byte{[]byte("spec: [this is not a valid patch")},
		Files: []*pb.File{{
			Path:    "/etc/kubernetes/extra/new.txt",
			Content: []byte("data"),
			Mode:    0o600,
			Owner:   "root:root",
		}},
	}
	_, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, statErr := os.Stat(newFile)
	assert.True(t, os.IsNotExist(statErr), "no file should be staged when the in-memory computation fails")
	assert.Empty(t, listPatchBackups(t, srv))
}

func TestPatchAPIServerManifest_PrunesOldBackups(t *testing.T) {
	// Each call makes a distinct manifest change, so each writes a new snapshot.
	// After more calls than the retention bound, the backup directory holds only
	// the newest maxAPIServerManifestBackups snapshots.
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusOK))
	seedPatchManifest(t, srv, baselinePatchManifest)
	srv.apiServerReadyMaxAttempts = 2
	srv.apiServerReadyPollInterval = 5 * time.Millisecond

	for i := range maxAPIServerManifestBackups + 3 {
		req := &pb.PatchAPIServerManifestRequest{
			FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{
				Name:  "authentication-config",
				Value: fmt.Sprintf("/etc/kubernetes/oidc/auth-%d.yaml", i),
			}}}},
		}
		resp, err := srv.PatchAPIServerManifest(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_APPLIED, resp.GetOutcome())
	}

	assert.Len(t, listPatchBackups(t, srv), maxAPIServerManifestBackups)
}

func TestPatchAPIServerManifest_RemovesFilesWhenManifestUnchanged(t *testing.T) {
	// Retry scenario: the manifest and staged files already match, but a
	// remove_files entry still exists on disk. Deleting a file the manifest no
	// longer references cannot affect the apiserver, so the removal must happen
	// and must NOT be gated on readiness — assert it even with /readyz failing.
	srv := newPatchTestServer(t, okReadyzHandler(http.StatusServiceUnavailable))
	seedPatchManifest(t, srv, baselinePatchManifest)
	srv.apiServerReadyMaxAttempts = 2
	srv.apiServerReadyPollInterval = 5 * time.Millisecond

	stale := filepath.Join(srv.RootPath, "/etc/kubernetes/oidc/old.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(stale), 0o755))
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))

	req := &pb.PatchAPIServerManifestRequest{RemoveFiles: []string{"/etc/kubernetes/oidc/old.yaml"}}

	resp, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_APPLIED, resp.GetOutcome())
	_, statErr := os.Stat(stale)
	assert.True(t, os.IsNotExist(statErr), "remove_files must be deleted even when the manifest is unchanged and /readyz is failing")

	// A genuine no-op (nothing left to remove) still returns UNCHANGED.
	resp2, err := srv.PatchAPIServerManifest(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, pb.PatchAPIServerManifestResponse_OUTCOME_UNCHANGED, resp2.GetOutcome())
}

// ── test helpers ─────────────────────────────────────────────────────────────

// volumeNames returns the set of .spec.volumes[*].name in the manifest doc.
func volumeNames(doc map[string]interface{}) []string {
	spec, ok := doc["spec"].(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := spec["volumes"].([]interface{})
	if !ok {
		return nil
	}
	names := make([]string, 0, len(raw))
	for _, rv := range raw {
		vm, ok := rv.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := vm["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

func okReadyzHandler(statusCode int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(statusCode)
	})
}

func newPatchTestServer(t *testing.T, handler http.Handler) *NodeAgentServer {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	rootPath, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	return &NodeAgentServer{
		NodeStatus:     &NodeStatus{Role: ControlPlaneRole},
		clientRegistry: &fakeLocalClientRegistry{localClient: newTestClientset(t, httpServer.URL)},
		Persister:      &MockPersister{},
		FileWriter:     &MockFileWriter{},
		RootPath:       rootPath,
		kubeadmFileOps: fileOps{
			lookupUser: func(name string) (*user.User, error) {
				if name == "root" {
					return &user.User{Username: "root", Uid: "0"}, nil
				}
				return nil, user.UnknownUserError(name)
			},
			lookupGroup: func(name string) (*user.Group, error) {
				if name == "root" {
					return &user.Group{Name: "root", Gid: "0"}, nil
				}
				return nil, user.UnknownGroupError(name)
			},
			chown: func(string, int, int) error { return nil },
		},
	}
}

func seedPatchManifest(t *testing.T, srv *NodeAgentServer, manifest string) {
	t.Helper()
	manifestPath := filepath.Join(srv.RootPath, kubeAPIServerManifestPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(manifestPath), 0o755))
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o600))
}

func listPatchBackups(t *testing.T, srv *NodeAgentServer) []string {
	t.Helper()
	backupDir := filepath.Join(srv.RootPath, apiServerManifestBackupDir)
	entries, err := os.ReadDir(backupDir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func seedPatchMirrorPodSequence(t *testing.T, srv *NodeAgentServer, uids ...string) {
	t.Helper()
	require.NotEmpty(t, uids)

	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.NotEmpty(t, hostname)
	podName := "kube-apiserver-" + hostname

	reg, ok := srv.clientRegistry.(*fakeLocalClientRegistry)
	require.True(t, ok)
	fc := kubernetesfake.NewSimpleClientset()
	idx := atomic.Int32{}
	fc.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		i := int(idx.Add(1) - 1)
		if i >= len(uids) {
			i = len(uids) - 1
		}
		uid := uids[i]
		if uid == "" {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, podName)
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "kube-system", UID: types.UID(uid)},
		}, nil
	})
	reg.client = fc

	srv.apiServerReadyMaxAttempts = 10
	srv.apiServerReadyPollInterval = 5 * time.Millisecond
}
