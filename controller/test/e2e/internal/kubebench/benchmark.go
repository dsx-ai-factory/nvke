// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BenchmarkFiles maps each benchmark file's path under kube-bench's cfg/
// directory, at BenchmarkCommit, to its SHA-256. config.yaml carries the
// version and target mappings that name cis-2.0; the rest are the benchmark.
// Each call returns a new map.
func BenchmarkFiles() map[string]string {
	return map[string]string{
		"config.yaml":               "ea21967591825671755c87acab3280e2a3af4a9245dc9af839d938aca9f799ef",
		"cis-2.0/config.yaml":       "f8beefa9c5392e5c25d03e802294ad1852ac094d3bf3053903dad718f599e3b7",
		"cis-2.0/master.yaml":       "0b1b74801b86638646a9bfb83c1f1f861c96f0f9009a96f4dc966889cad48b8d",
		"cis-2.0/controlplane.yaml": "c675e598ab6414b2e2e1b4e8dc9d947dd2d3fd3138ad1107b07c83cfb4c96897",
		"cis-2.0/etcd.yaml":         "6febdc4959f769153ffe3b605a8b4db794b29ed605e11cbe651575d0f7d768a5",
		"cis-2.0/node.yaml":         "6e1ed27051bffcebbaeba5e6895c81704efd785e9daf8bb896febe8f135862a7",
		"cis-2.0/policies.yaml":     "716ac411115288c01553491b6fe96d85366c3d012af8bdc2abda9ae7d10884f9",
	}
}

const (
	// maxBenchmarkFile bounds one file's download. The largest is about 40 KB.
	maxBenchmarkFile = 1 << 20

	// benchmarkVolume is the scan pod's name for the benchmark files.
	benchmarkVolume = "kube-bench-cfg"

	// fetchAttempts and fetchRetryDelay retry a download that failed in
	// transit or with a server error. The nth retry waits n times the delay.
	fetchAttempts   = 3
	fetchRetryDelay = 200 * time.Millisecond
)

// statusError is a download answered with a status other than 200 OK.
type statusError struct {
	url    string
	status string
	code   int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: %s", e.url, e.status)
}

// retryable reports whether a failed download may succeed if repeated: a
// transport error, a server error, or rate limiting. A missing file or a
// file too large will not change.
func retryable(err error) bool {
	var status *statusError
	if errors.As(err, &status) {
		return status.code >= http.StatusInternalServerError || status.code == http.StatusTooManyRequests
	}
	var transport *url.Error
	return errors.As(err, &transport) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// FetchBenchmark downloads each file in want from baseURL, a URL ending in "/"
// that the file's path follows, and checks it against its SHA-256 in want. It
// returns the files by path, or the first error, naming the file.
func FetchBenchmark(ctx context.Context, client *http.Client, baseURL string, want map[string]string) (map[string][]byte, error) {
	files := make(map[string][]byte, len(want))
	for _, path := range slices.Sorted(maps.Keys(want)) {
		content, err := fetchWithRetry(ctx, client, baseURL+path)
		if err != nil {
			return nil, fmt.Errorf("fetching kube-bench benchmark file %s: %w", path, err)
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != want[path] {
			return nil, fmt.Errorf("kube-bench benchmark file %s has SHA-256 %s, want %s", path, got, want[path])
		}
		files[path] = content
	}
	return files, nil
}

// fetchWithRetry downloads one file, retrying what retryable allows.
func fetchWithRetry(ctx context.Context, client *http.Client, fileURL string) ([]byte, error) {
	var err error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		var content []byte
		content, err = fetchBenchmarkFile(ctx, client, fileURL)
		if err == nil || !retryable(err) || attempt == fetchAttempts {
			return content, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * fetchRetryDelay):
		}
	}
	return nil, err
}

func fetchBenchmarkFile(ctx context.Context, client *http.Client, fileURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{url: fileURL, status: resp.Status, code: resp.StatusCode}
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxBenchmarkFile+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", fileURL, err)
	}
	if len(content) > maxBenchmarkFile {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", fileURL, maxBenchmarkFile)
	}
	return content, nil
}

// BenchmarkConfigMap holds the benchmark files for the scan pods to mount. A
// ConfigMap key cannot contain "/", so each file's key is its path with "/"
// replaced by "_"; BenchmarkVolume maps the keys back.
func BenchmarkConfigMap(namespace string, files map[string][]byte) *corev1.ConfigMap {
	data := make(map[string]string, len(files))
	for path, content := range files {
		data[configMapKey(path)] = string(content)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-bench-cfg-",
			Namespace:    namespace,
			Labels:       map[string]string{PodLabel: PodLabelValue},
		},
		Data: data,
	}
}

// BenchmarkVolume mounts the ConfigMap BenchmarkConfigMap made, once created
// under configMap, with every file back at its path under cfg/.
func BenchmarkVolume(configMap string, files map[string][]byte) corev1.Volume {
	items := make([]corev1.KeyToPath, 0, len(files))
	for _, path := range slices.Sorted(maps.Keys(files)) {
		items = append(items, corev1.KeyToPath{Key: configMapKey(path), Path: path})
	}
	return corev1.Volume{
		Name: benchmarkVolume,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
			Items:                items,
		}},
	}
}

func configMapKey(path string) string {
	return strings.ReplaceAll(path, "/", "_")
}
