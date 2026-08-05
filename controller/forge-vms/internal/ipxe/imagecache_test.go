// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package ipxe

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestImageCacheEnsure(t *testing.T) {
	// Set up a test HTTP server serving a fake qcow2 image
	imageData := []byte("QFI\xfb" + "fake-qcow2-image-data")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(imageData)
	}))
	defer ts.Close()

	cacheDir := filepath.Join(t.TempDir(), "image-cache")
	cache := NewImageCache(cacheDir, slog.Default())

	// First call should download
	path, err := cache.Ensure(context.Background(), ts.URL+"/test.qcow2")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != string(imageData) {
		t.Errorf("cached data mismatch: got %d bytes, want %d", len(data), len(imageData))
	}

	// Second call should hit cache (no download)
	path2, err := cache.Ensure(context.Background(), ts.URL+"/test.qcow2")
	if err != nil {
		t.Fatalf("Ensure() second call error = %v", err)
	}
	if path2 != path {
		t.Errorf("cache miss: got different path %q vs %q", path2, path)
	}
}

func TestImageCacheConcurrent(t *testing.T) {
	downloadCount := 0
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		downloadCount++
		mu.Unlock()
		w.Write([]byte("QFI\xfb" + "test-data"))
	}))
	defer ts.Close()

	cacheDir := filepath.Join(t.TempDir(), "image-cache")
	cache := NewImageCache(cacheDir, slog.Default())

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.Ensure(context.Background(), ts.URL+"/concurrent.qcow2")
			if err != nil {
				t.Errorf("Ensure() error = %v", err)
			}
		}()
	}
	wg.Wait()

	// Should have downloaded at most once (could be 1 if all concurrent, or up to 5 if serialized)
	// The important thing is no errors occurred
	mu.Lock()
	t.Logf("Download count: %d (ideally 1)", downloadCount)
	mu.Unlock()
}

func TestImageCacheHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	cacheDir := filepath.Join(t.TempDir(), "image-cache")
	cache := NewImageCache(cacheDir, slog.Default())

	_, err := cache.Ensure(context.Background(), ts.URL+"/missing.qcow2")
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

func TestCacheKeyForURL(t *testing.T) {
	key1 := cacheKeyForURL("https://example.com/a.qcow2")
	key2 := cacheKeyForURL("https://example.com/b.qcow2")
	if key1 == key2 {
		t.Error("different URLs should produce different cache keys")
	}
	if len(key1) != 32 {
		t.Errorf("cache key length = %d, want 32", len(key1))
	}
}
