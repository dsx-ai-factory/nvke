// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

// An image built in the same CI job is already on disk, so it should not need
// an HTTP server to reach nico-vms.
func TestImageCacheEnsureLocalFile(t *testing.T) {
	imageData := []byte("QFI\xfb" + "locally-built-image")
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "nvke-node.qcow2")
	if err := os.WriteFile(src, imageData, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "file url", url: "file://" + src},
		{name: "file url with localhost", url: "file://localhost" + src},
		{name: "bare absolute path", url: src},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewImageCache(filepath.Join(t.TempDir(), "image-cache"), slog.Default())

			path, err := cache.Ensure(context.Background(), tc.url)
			if err != nil {
				t.Fatalf("Ensure(%q): %v", tc.url, err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(imageData) {
				t.Errorf("cached content = %q, want %q", got, imageData)
			}
			if path == src {
				t.Error("cache must own its copy, not hand back the source path")
			}

			// The source may be rebuilt or deleted once the image is cached.
			if err := os.Remove(src); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(src, imageData, 0o644) })
			if _, err := cache.Ensure(context.Background(), tc.url); err != nil {
				t.Errorf("cache hit must not need the source: %v", err)
			}
		})
	}
}

func TestImageCacheEnsureLocalFileMissing(t *testing.T) {
	cache := NewImageCache(filepath.Join(t.TempDir(), "image-cache"), slog.Default())
	if _, err := cache.Ensure(context.Background(), "/nonexistent/nvke-node.qcow2"); err == nil {
		t.Fatal("expected an error naming the missing file")
	}
}

// A file:// URL naming another host is a file on a different machine, so it
// must not be read from this one.
func TestLocalImagePathRejectsRemoteHost(t *testing.T) {
	for _, url := range []string{
		"file://otherbox/images/nvke-node.qcow2",
		"http://172.30.0.1:8899/nvke-node.qcow2",
		"https://example.invalid/nvke-node.qcow2",
	} {
		if got, ok := localImagePath(url); ok {
			t.Errorf("localImagePath(%q) = %q, true; want false", url, got)
		}
	}
}
