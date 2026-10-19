// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apiserver/pkg/util/configmetrics"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kubemetrics "k8s.io/component-base/metrics"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const reloadTestIdentity = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func reloadTestConfigPath(dir string) string {
	return filepath.Join(dir, "etc/kubernetes/kube-api-encryption/encryption-config.yaml")
}

func reloadTestMetrics(configHash string, process, count int) []byte {
	return []byte(fmt.Sprintf("process_start_time_seconds %d\napiserver_encryption_config_controller_last_config_info{apiserver_id_hash=%q,hash=%q} 1\napiserver_encryption_config_controller_automatic_reloads_total{apiserver_id_hash=%q,status=\"success\"} %d\n", process, reloadTestIdentity, configHash, reloadTestIdentity, count))
}

func reloadTestServer(t *testing.T, dir string, handler http.HandlerFunc) *NodeAgentServer {
	t.Helper()
	endpoint := httptest.NewServer(handler)
	t.Cleanup(endpoint.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: endpoint.URL})
	require.NoError(t, err)
	// The real RPC uses its real local client registry, WAL and config files.
	return &NodeAgentServer{
		RootPath: dir, NodeStatus: &NodeStatus{Role: ControlPlaneRole},
		clientRegistry: &reloadTestRegistry{client: client}, reloadPollInterval: time.Millisecond,
	}
}

type reloadTestRegistry struct {
	kubernetesClientRegistry
	client *kubernetes.Clientset
}

func (r *reloadTestRegistry) LocalClient() (*kubernetes.Clientset, error) { return r.client, nil }

func reloadTestPending(t *testing.T) (string, *pb.UpdateEncryptionConfigRequest, string) {
	t.Helper()
	dir := t.TempDir()
	writeEncryptionConfigFixture(t, dir, []KeyConfig{
		{Name: "current-primary-encryption-key", Secret: testKey1},
		{Name: "kube-api-encryption-key-1", Secret: testKey2},
	})
	req := &pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
		AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
	}}
	// Kubernetes computeEncryptionConfigHash hashes the exact file bytes, not
	// re-marshaled YAML. Derive that fixture with the standard library directly.
	// Legacy two-field WAL fixtures remain unchanged.
	raw, err := os.ReadFile(reloadTestConfigPath(dir))
	require.NoError(t, err)
	return dir, req, fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

func TestEncryptionReload_ResumesAfterAPIServerRestart(t *testing.T) {
	dir, req, loadedHash := reloadTestPending(t)
	var requests atomic.Int32
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/metrics", r.URL.Path)
		requests.Add(1)
		_, _ = w.Write(reloadTestMetrics(loadedHash, 200, 0))
	})
	writeWALFixture(t, server, 5, req)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := server.UpdateEncryptionConfig(ctx, req)
	require.NoError(t, err, "startup loaded config must resolve a baseline from the old process")
	require.EqualValues(t, 2, requests.Load(), "stable config and process are confirmed twice")
	require.False(t, walFileExists(server))
	// A late same-operation retry rechecks loaded state without another write.
	_, err = server.UpdateEncryptionConfig(ctx, req)
	require.NoError(t, err)
	require.EqualValues(t, 4, requests.Load())
}

func TestEncryptionReload_UsesKubernetesCollectorAndHash(t *testing.T) {
	dir, req, loadedHash := reloadTestPending(t)
	provider := configmetrics.NewAtomicHashProvider()
	provider.SetHashes(reloadTestIdentity, loadedHash)
	registry := kubemetrics.NewKubeRegistry()
	desc := kubemetrics.NewDesc("apiserver_encryption_config_controller_last_config_info", "Loaded encryption config", []string{"apiserver_id_hash", "hash"}, nil, kubemetrics.ALPHA, "")
	require.NoError(t, registry.CustomRegister(configmetrics.NewConfigInfoCustomCollector(desc, provider)))
	families, err := registry.Gather()
	require.NoError(t, err)
	var output bytes.Buffer
	for _, family := range families {
		_, err := expfmt.MetricFamilyToText(&output, family)
		require.NoError(t, err)
	}
	// Successful CounterVec children may be absent at startup. The real custom
	// collector exports only current config, not a retained history of hashes.
	output.WriteString("process_start_time_seconds 200\n")
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(output.Bytes())
	})
	writeWALFixture(t, server, 5, req)
	_, err = server.UpdateEncryptionConfig(t.Context(), req)
	require.NoError(t, err)
	require.False(t, walFileExists(server))
}

func TestEncryptionReload_RejectsStaleOrInvalidProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"invalid fingerprint", func(raw []byte) []byte {
			return bytes.ReplaceAll(raw, []byte(`hash="sha256:`), []byte(`hash="sha256:bbbb`))
		}},
		{"missing loaded config", func(raw []byte) []byte { return []byte(strings.Split(string(raw), "\n")[0] + "\n") }},
		{"counter alone", func([]byte) []byte {
			return []byte("apiserver_encryption_config_controller_automatic_reloads_total{status=\"success\"} 999\n")
		}},
		{"missing process", func(raw []byte) []byte { return []byte(strings.Join(strings.Split(string(raw), "\n")[1:], "\n")) }},
		{"zero process", func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte("seconds 200"), []byte("seconds 0")) }},
		{"infinite process", func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte("seconds 200"), []byte("seconds +Inf")) }},
		{"NaN info", func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte("} 1\n"), []byte("} NaN\n")) }},
		{"info is not one", func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte("} 1\n"), []byte("} 2\n")) }},
		{"duplicate process", func(raw []byte) []byte { return append(raw, []byte("process_start_time_seconds 200\n")...) }},
		{"duplicate config", func(raw []byte) []byte { return append(raw, []byte(strings.Split(string(raw), "\n")[1]+"\n")...) }},
		{"conflicting config", func(raw []byte) []byte {
			line := strings.Split(string(raw), "\n")[1]
			return append(raw, []byte(strings.Replace(line, "sha256:", "sha256:bbbb", 1)+"\n")...)
		}},
		{"info has wrong type", func(raw []byte) []byte {
			return append([]byte("# TYPE apiserver_encryption_config_controller_last_config_info counter\n"), raw...)
		}},
		{"process has wrong type", func(raw []byte) []byte {
			return append([]byte("# TYPE process_start_time_seconds counter\n"), raw...)
		}},
		{"malformed exposition", func(raw []byte) []byte { return append(raw, []byte("invalid{hash=\"private-material 1\n")...) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, req, loadedHash := reloadTestPending(t)
			raw := tc.edit(reloadTestMetrics(loadedHash, 200, 999))
			server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) })
			writeWALFixture(t, server, 5, req)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			_, err := server.UpdateEncryptionConfig(ctx, req)
			require.Equal(t, codes.DeadlineExceeded, status.Code(err))
			require.NotContains(t, err.Error(), loadedHash)
			require.NotContains(t, err.Error(), "private-material")
			saved, exists, err := readEncRotationWAL(server.encRotationWALPath())
			require.NoError(t, err)
			require.True(t, exists)
			require.Equal(t, 5, saved.ReloadCountBefore)
		})
	}
}

func TestEncryptionReload_StaleValidConfigCannotUseHigherCounter(t *testing.T) {
	dir, req, _ := reloadTestPending(t)
	staleHash := "sha256:" + strings.Repeat("b", 64)
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(reloadTestMetrics(staleHash, 200, 999))
	})
	writeWALFixture(t, server, 5, req)
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		_, err := server.UpdateEncryptionConfig(ctx, req)
		cancel()
		require.Equal(t, codes.DeadlineExceeded, status.Code(err))
		require.True(t, walFileExists(server))
	}
}

func TestEncryptionReload_RechecksProcessAndConfig(t *testing.T) {
	for _, changing := range []string{"process", "config", "apiserver identity"} {
		t.Run(changing, func(t *testing.T) {
			dir, req, loadedHash := reloadTestPending(t)
			var calls atomic.Int32
			server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
				call := calls.Add(1)
				process, hash := 200, loadedHash
				if call%2 == 0 {
					if changing == "process" {
						process = 300
					} else if changing == "config" {
						hash = "sha256:" + strings.Repeat("b", 64)
					}
				}
				raw := reloadTestMetrics(hash, process, 999)
				if changing == "apiserver identity" && call%2 == 0 {
					raw = bytes.ReplaceAll(raw, []byte(reloadTestIdentity), []byte("sha256:"+strings.Repeat("c", 64)))
				}
				_, _ = w.Write(raw)
			})
			writeWALFixture(t, server, 5, req)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			_, err := server.UpdateEncryptionConfig(ctx, req)
			require.Equal(t, codes.DeadlineExceeded, status.Code(err))
			require.GreaterOrEqual(t, calls.Load(), int32(2))
			require.True(t, walFileExists(server))
		})
	}
}

func TestEncryptionReload_StableNewProcessAfterOneRaceSucceeds(t *testing.T) {
	dir, req, loadedHash := reloadTestPending(t)
	var calls atomic.Int32
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		process := 200
		if calls.Add(1) > 1 {
			process = 300
		}
		_, _ = w.Write(reloadTestMetrics(loadedHash, process, 0))
	})
	writeWALFixture(t, server, 5, req)
	_, err := server.UpdateEncryptionConfig(t.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, 4, calls.Load())
	require.False(t, walFileExists(server))
}

func TestEncryptionReload_FreshWriteRequiresUsableIdentity(t *testing.T) {
	for _, proof := range []string{"missing", "stale"} {
		t.Run(proof, func(t *testing.T) {
			dir := t.TempDir()
			writeEncryptionConfigFixture(t, dir, []KeyConfig{{Name: "current-primary-encryption-key", Secret: testKey1}})
			before, err := os.ReadFile(reloadTestConfigPath(dir))
			require.NoError(t, err)
			server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
				raw := []byte("apiserver_encryption_config_controller_automatic_reload_success_total{apiserver_id_hash=\"old\"} 999\n")
				if proof == "stale" {
					raw = reloadTestMetrics("sha256:"+strings.Repeat("b", 64), 200, 999)
				}
				_, _ = w.Write(raw)
			})
			_, req, _ := reloadTestPending(t)
			_, err = server.UpdateEncryptionConfig(t.Context(), req)
			require.Error(t, err)
			after, err := os.ReadFile(reloadTestConfigPath(dir))
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.False(t, walFileExists(server))
		})
	}
}

func TestEncryptionReload_MetricsDeadlinePreservesCheckpoint(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh=%v", fresh), func(t *testing.T) {
			dir, req, _ := reloadTestPending(t)
			if fresh {
				writeEncryptionConfigFixture(t, dir, []KeyConfig{{Name: "current-primary-encryption-key", Secret: testKey1}})
			}
			server := reloadTestServer(t, dir, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			if !fresh {
				writeWALFixture(t, server, 5, req)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err := server.UpdateEncryptionConfig(ctx, req)
			require.Equal(t, codes.DeadlineExceeded, status.Code(err))
			require.Less(t, time.Since(started), time.Second, "probe must honor the caller deadline")
			require.Equal(t, !fresh, walFileExists(server))
		})
	}
}

func TestEncryptionReload_DiskOrOwnershipChangesPreserveWAL(t *testing.T) {
	for _, changing := range []string{"disk", "ownership"} {
		t.Run(changing, func(t *testing.T) {
			dir, req, loadedHash := reloadTestPending(t)
			var calls atomic.Int32
			var server *NodeAgentServer
			server = reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					if changing == "disk" {
						require.NoError(t, os.WriteFile(reloadTestConfigPath(dir), []byte("unexpected external config"), 0o600))
					} else {
						require.NoError(t, writeEncRotationWAL(server.encRotationWALPath(), encRotationWALEntry{ReloadCountBefore: 123, OpID: "different-operation"}))
					}
				}
				_, _ = w.Write(reloadTestMetrics(loadedHash, 200, 0))
			})
			writeWALFixture(t, server, 5, req)
			_, err := server.UpdateEncryptionConfig(t.Context(), req)
			require.Equal(t, codes.FailedPrecondition, status.Code(err))
			require.True(t, walFileExists(server))
			saved, _, err := readEncRotationWAL(server.encRotationWALPath())
			require.NoError(t, err)
			if changing == "ownership" {
				require.Equal(t, "different-operation", saved.OpID)
				require.Equal(t, 123, saved.ReloadCountBefore)
			}
		})
	}
}

func TestEncryptionReload_WALRemovalFailureIsNotMetricsOutage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory used to make WAL removal fail")
	}
	dir, req, loadedHash := reloadTestPending(t)
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(reloadTestMetrics(loadedHash, 200, 0))
	})
	writeWALFixture(t, server, 5, req)
	walDir := filepath.Dir(server.encRotationWALPath())
	require.NoError(t, os.Chmod(walDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(walDir, 0o700) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := server.UpdateEncryptionConfig(ctx, req)
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, "remove rotation WAL")
	require.NotContains(t, err.Error(), "metrics")
	require.True(t, walFileExists(server))
}

func TestEncryptionReload_ConcurrentSameOperationRetries(t *testing.T) {
	dir, req, loadedHash := reloadTestPending(t)
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(reloadTestMetrics(loadedHash, 200, 0))
	})
	writeWALFixture(t, server, 5, req)
	var group sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		group.Go(func() {
			_, err := server.UpdateEncryptionConfig(t.Context(), req)
			results <- err
		})
	}
	group.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.False(t, walFileExists(server))
	config, err := server.readEncryptionConfig()
	require.NoError(t, err)
	require.True(t, config.IsInDesiredState(req))
}

func TestEncryptionReload_AlreadyDesiredWithoutWALRequiresLiveProof(t *testing.T) {
	for _, proof := range []string{"stale", "missing", "process changes"} {
		t.Run(proof, func(t *testing.T) {
			dir, req, loadedHash := reloadTestPending(t)
			var calls atomic.Int32
			server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
				process := 200
				if proof == "process changes" && calls.Add(1)%2 == 0 {
					process = 300
				}
				hash := loadedHash
				if proof == "stale" {
					hash = "sha256:" + strings.Repeat("b", 64)
				}
				raw := reloadTestMetrics(hash, process, 999)
				if proof == "missing" {
					raw = nil
				}
				_, _ = w.Write(raw)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			_, err := server.UpdateEncryptionConfig(ctx, req)
			require.Equal(t, codes.DeadlineExceeded, status.Code(err))
			require.False(t, walFileExists(server), "confirmation must not manufacture a checkpoint")
		})
	}
}

func TestEncryptionReload_ForeignCheckpointWrittenDuringFinalProbe(t *testing.T) {
	dir, req, loadedHash := reloadTestPending(t)
	var server *NodeAgentServer
	var calls atomic.Int32
	server = reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 2 {
			require.NoError(t, writeEncRotationWAL(server.encRotationWALPath(), encRotationWALEntry{ReloadCountBefore: 123, OpID: "different-operation"}))
		}
		_, _ = w.Write(reloadTestMetrics(loadedHash, 200, 0))
	})
	writeWALFixture(t, server, 5, req)
	_, err := server.UpdateEncryptionConfig(t.Context(), req)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	saved, exists, err := readEncRotationWAL(server.encRotationWALPath())
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "different-operation", saved.OpID)
	require.Equal(t, 123, saved.ReloadCountBefore)
}

func TestEncryptionReload_MetricsOutageDoesNotExposeResponseBody(t *testing.T) {
	dir, req, loadedHash := reloadTestPending(t)
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, loadedHash+" private-material", http.StatusServiceUnavailable)
	})
	writeWALFixture(t, server, 5, req)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := server.UpdateEncryptionConfig(ctx, req)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	require.NotContains(t, err.Error(), loadedHash)
	require.NotContains(t, err.Error(), "private-material")
	require.True(t, walFileExists(server))
}

func TestEncryptionReload_CancelBeforeMutation(t *testing.T) {
	dir, req, _ := reloadTestPending(t)
	var calls atomic.Int32
	server := reloadTestServer(t, dir, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write(nil)
	})
	writeWALFixture(t, server, 5, req)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := server.UpdateEncryptionConfig(ctx, req)
	require.Equal(t, codes.Canceled, status.Code(err))
	require.Zero(t, calls.Load())
	require.True(t, walFileExists(server))
}

func TestEncryptionReload_IdentityParsingIgnoresReloadCounters(t *testing.T) {
	_, _, loadedHash := reloadTestPending(t)
	legacyCounter := fmt.Sprintf("apiserver_encryption_config_controller_automatic_reload_success_total{apiserver_id_hash=%q} 9\n", reloadTestIdentity)
	for _, tc := range []struct {
		name         string
		edit         func([]byte) []byte
		wantAPIServe string
	}{
		{"current counter", func(raw []byte) []byte { return raw }, reloadTestIdentity},
		{"no counter at startup", func(raw []byte) []byte {
			return []byte(strings.Join(strings.Split(string(raw), "\n")[:2], "\n") + "\n")
		}, reloadTestIdentity},
		{"disagreeing counter names", func(raw []byte) []byte { return append(raw, []byte(legacyCounter)...) }, reloadTestIdentity},
		{"malformed counter", func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte("} 7\n"), []byte("} -1.5\n")) }, reloadTestIdentity},
		{"counter for another identity", func(raw []byte) []byte {
			return bytes.ReplaceAll(raw, []byte(`reloads_total{apiserver_id_hash="`+reloadTestIdentity), []byte(`reloads_total{apiserver_id_hash="sha256:`+strings.Repeat("c", 64)))
		}, reloadTestIdentity},
		{"disabled apiserver identity", func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte(reloadTestIdentity), nil) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := parseApiserverEncryptionState(tc.edit(reloadTestMetrics(loadedHash, 200, 7)))
			require.NoError(t, err)
			require.Equal(t, loadedHash, state.configHash)
			require.Equal(t, tc.wantAPIServe, state.apiserverID)
			require.EqualValues(t, 200, state.processStarted)
		})
	}
}
