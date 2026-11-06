// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

// syntheticBenchmark stands in for kube-bench's cfg/ tree: the same paths,
// with content of our own.
func syntheticBenchmark() map[string][]byte {
	files := map[string][]byte{}
	for path := range BenchmarkFiles() {
		files[path] = []byte("# synthetic " + path + "\n")
	}
	return files
}

func checksums(files map[string][]byte) map[string]string {
	sums := map[string]string{}
	for path, content := range files {
		sum := sha256.Sum256(content)
		sums[path] = hex.EncodeToString(sum[:])
	}
	return sums
}

// benchmarkServer serves files under /cfg/, and 404 for anything else.
func benchmarkServer(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, ok := files[strings.TrimPrefix(r.URL.Path, "/cfg/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchBenchmark(t *testing.T) {
	t.Parallel()

	files := syntheticBenchmark()
	want := checksums(files)

	tests := []struct {
		name    string
		serve   func() map[string][]byte
		cancel  bool
		wantErr []string
	}{
		{
			name:  "every file is fetched and checked",
			serve: func() map[string][]byte { return files },
		},
		{
			name: "a checksum mismatch is an error naming the file",
			serve: func() map[string][]byte {
				changed := maps.Clone(files)
				changed["cis-2.0/master.yaml"] = []byte("<html>not the benchmark</html>")
				return changed
			},
			wantErr: []string{"cis-2.0/master.yaml", "SHA-256"},
		},
		{
			name: "a missing file is an error naming the file and the status",
			serve: func() map[string][]byte {
				missing := maps.Clone(files)
				delete(missing, "cis-2.0/node.yaml")
				return missing
			},
			wantErr: []string{"cis-2.0/node.yaml", "404"},
		},
		{
			name:    "a canceled context stops the fetch",
			serve:   func() map[string][]byte { return files },
			cancel:  true,
			wantErr: []string{"context canceled"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := benchmarkServer(t, tc.serve())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			got, err := FetchBenchmark(ctx, server.Client(), server.URL+"/cfg/", want)
			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, fragment := range tc.wantErr {
					assert.Contains(t, err.Error(), fragment)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, files, got)
		})
	}
}

func TestBenchmarkFiles(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{
		"cis-2.0/config.yaml",
		"cis-2.0/controlplane.yaml",
		"cis-2.0/etcd.yaml",
		"cis-2.0/master.yaml",
		"cis-2.0/node.yaml",
		"cis-2.0/policies.yaml",
		"config.yaml",
	}, slices.Sorted(maps.Keys(BenchmarkFiles())))
	hexSHA256 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for path, sum := range BenchmarkFiles() {
		assert.Regexp(t, hexSHA256, sum, path)
	}
	assert.True(t, strings.HasSuffix(BenchmarkURL, BenchmarkCommit+"/cfg/"), BenchmarkURL)
}

func TestBenchmarkConfigMap(t *testing.T) {
	t.Parallel()

	files := syntheticBenchmark()
	configMap := BenchmarkConfigMap("default", files)

	assert.Equal(t, "default", configMap.Namespace)
	assert.Equal(t, "kube-bench-cfg-", configMap.GenerateName)
	assert.Equal(t, PodLabelValue, configMap.Labels[PodLabel])
	require.Len(t, configMap.Data, len(files))
	for key := range configMap.Data {
		assert.Empty(t, validation.IsConfigMapKey(key), "key %q", key)
	}

	volume := BenchmarkVolume("kube-bench-cfg-x7k2p", files)
	require.NotNil(t, volume.ConfigMap)
	assert.Equal(t, "kube-bench-cfg-x7k2p", volume.ConfigMap.Name)

	// The items rebuild cfg/'s layout from the ConfigMap's keys.
	rebuilt := map[string][]byte{}
	for _, item := range volume.ConfigMap.Items {
		content, ok := configMap.Data[item.Key]
		require.True(t, ok, "item key %q is not in the ConfigMap", item.Key)
		rebuilt[item.Path] = []byte(content)
	}
	assert.Equal(t, files, rebuilt)
}

// flakyServer answers each path's first request with status, then serves it.
func flakyServer(t *testing.T, files map[string][]byte, status int) (*httptest.Server, *sync.Map) {
	t.Helper()
	requests := &sync.Map{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/cfg/")
		count, _ := requests.LoadOrStore(path, new(atomic.Int32))
		if count.(*atomic.Int32).Add(1) == 1 {
			w.WriteHeader(status)
			return
		}
		content, ok := files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func TestFetchBenchmarkRetries(t *testing.T) {
	t.Parallel()

	files := syntheticBenchmark()
	want := checksums(files)

	t.Run("a transient 503 is retried", func(t *testing.T) {
		t.Parallel()
		server, requests := flakyServer(t, files, http.StatusServiceUnavailable)
		got, err := FetchBenchmark(t.Context(), server.Client(), server.URL+"/cfg/", want)
		require.NoError(t, err)
		assert.Equal(t, files, got)
		count, _ := requests.Load("config.yaml")
		assert.Equal(t, int32(2), count.(*atomic.Int32).Load())
	})
	t.Run("a 404 is not retried", func(t *testing.T) {
		t.Parallel()
		server, requests := flakyServer(t, files, http.StatusNotFound)
		_, err := FetchBenchmark(t.Context(), server.Client(), server.URL+"/cfg/", want)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
		count, _ := requests.Load("cis-2.0/config.yaml")
		assert.Equal(t, int32(1), count.(*atomic.Int32).Load())
	})
}
