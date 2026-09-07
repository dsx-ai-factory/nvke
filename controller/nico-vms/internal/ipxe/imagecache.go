// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ipxe

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ImageCache manages a local cache of qcow2 images downloaded from URLs.
// Multiple callers requesting the same URL will share a single download.
type ImageCache struct {
	CacheDir string
	logger   *slog.Logger
	mu       sync.Mutex
	inflight map[string]*downloadResult
}

type downloadResult struct {
	done chan struct{}
	path string
	err  error
}

// NewImageCache creates a new ImageCache with the given cache directory.
func NewImageCache(cacheDir string, logger *slog.Logger) *ImageCache {
	return &ImageCache{
		CacheDir: cacheDir,
		logger:   logger,
		inflight: make(map[string]*downloadResult),
	}
}

// Ensure returns the local path to a cached copy of the image at imageURL.
// If the image is not cached, it downloads it. Concurrent calls for the same
// URL share a single download.
func (c *ImageCache) Ensure(ctx context.Context, imageURL string) (string, error) {
	if err := os.MkdirAll(c.CacheDir, 0o755); err != nil {
		return "", fmt.Errorf("imagecache: create cache dir: %w", err)
	}

	cacheKey := cacheKeyForURL(imageURL)
	localPath := filepath.Join(c.CacheDir, cacheKey+".qcow2")

	// Fast path: already cached
	if info, err := os.Stat(localPath); err == nil && info.Size() > 0 {
		c.logger.Info("Image cache hit", "url", imageURL, "path", localPath)
		return localPath, nil
	}

	// Check for inflight download or start one
	c.mu.Lock()
	if result, ok := c.inflight[cacheKey]; ok {
		c.mu.Unlock()
		select {
		case <-result.done:
			return result.path, result.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	result := &downloadResult{done: make(chan struct{})}
	c.inflight[cacheKey] = result
	c.mu.Unlock()

	// Perform the download
	result.path, result.err = c.download(ctx, imageURL, localPath)
	close(result.done)

	// Clean up inflight entry
	c.mu.Lock()
	delete(c.inflight, cacheKey)
	c.mu.Unlock()

	return result.path, result.err
}

func (c *ImageCache) download(ctx context.Context, imageURL, localPath string) (string, error) {
	// An image built in the same job is already on disk. Copying it costs one
	// pass over a file the caller already has, and saves standing up an HTTP
	// server whose only client is on the same machine.
	if src, ok := localImagePath(imageURL); ok {
		return c.copyLocal(src, imageURL, localPath)
	}

	c.logger.Info("Downloading image", "url", imageURL, "dest", localPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", fmt.Errorf("imagecache: create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("imagecache: download %s: %w", imageURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imagecache: download %s: HTTP %d", imageURL, resp.StatusCode)
	}

	tmpPath := localPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("imagecache: create temp file: %w", err)
	}

	written, err := io.Copy(f, resp.Body)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("imagecache: write %s: %w", localPath, err)
	}

	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("imagecache: rename temp to final: %w", err)
	}

	c.logger.Info("Image downloaded", "url", imageURL, "path", localPath, "bytes", written)
	return localPath, nil
}

// localImagePath resolves imageURL to a path on this machine, for a "file://"
// URL or a bare absolute path. It reports false for anything else, including a
// file:// URL naming a host, which would mean a file on a different machine.
func localImagePath(imageURL string) (string, bool) {
	if strings.HasPrefix(imageURL, "/") {
		return imageURL, true
	}
	u, err := url.Parse(imageURL)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", false
	}
	return u.Path, true
}

// copyLocal copies a local image into the cache, through a temporary file and a
// rename so a cache entry is never half-written. The copy is deliberate: the
// cached file becomes a VM's read-only backing file and outlives the build that
// produced the source, so the cache owns its own copy.
func (c *ImageCache) copyLocal(src, imageURL, localPath string) (string, error) {
	c.logger.Info("Copying local image", "src", src, "dest", localPath)

	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("imagecache: stat local image %s: %w", src, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("imagecache: local image %s is a directory", src)
	}

	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("imagecache: open local image %s: %w", src, err)
	}
	defer in.Close()

	tmpPath := localPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("imagecache: create temp file: %w", err)
	}

	written, err := io.Copy(out, in)
	if closeErr := out.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("imagecache: copy %s: %w", src, err)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("imagecache: rename temp to final: %w", err)
	}

	c.logger.Info("Local image copied", "url", imageURL, "path", localPath, "bytes", written)
	return localPath, nil
}

// cacheKeyForURL returns a stable cache key for the given image URL.
// For localhost/127.0.0.1 URLs (test servers with random ports), the key
// is derived from the path only, so the same image served on different
// ports across test runs shares one cached copy.
func cacheKeyForURL(imageURL string) string {
	key := imageURL
	if u, err := url.Parse(imageURL); err == nil {
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			key = u.Path
		}
	}
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16])
}
