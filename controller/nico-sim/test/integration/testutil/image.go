// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	DefaultBaseImageRelPath = "images/ubuntu-cloud.qcow2"
	qcow2Magic              = "QFI\xfb"
)

func DefaultBaseImageURL() string {
	if runtime.GOARCH == "arm64" {
		return "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
	}
	return "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
}

// FindRepoRoot walks up from the current working directory to find the
// nico-sim module root (the directory containing go.mod).
func FindRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in any parent of %s", wd)
		}
		dir = parent
	}
}

// EnsureDefaultBaseImage downloads the Ubuntu cloud image to the conventional
// cache location (images/ubuntu-cloud.qcow2 relative to the repo root) if it
// does not already exist. Returns the absolute path to the image.
func EnsureDefaultBaseImage(ctx context.Context) (string, error) {
	repoRoot, err := FindRepoRoot()
	if err != nil {
		return "", err
	}
	return EnsureBaseImage(ctx, filepath.Join(repoRoot, DefaultBaseImageRelPath))
}

// EnsureBaseImage ensures a qcow2 base image exists at the given path,
// downloading from the default Ubuntu cloud image URL if necessary.
func EnsureBaseImage(ctx context.Context, imagePath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		return "", fmt.Errorf("create image cache directory: %w", err)
	}

	if info, err := os.Stat(imagePath); err == nil && info.Size() > 0 {
		if err := ValidateQCOW2(imagePath); err == nil {
			fmt.Fprintf(os.Stderr, "nico-sim: reusing cached base image: %s\n", imagePath)
			return imagePath, nil
		}
		_ = os.Remove(imagePath)
	}

	imageURL := DefaultBaseImageURL()
	fmt.Fprintf(os.Stderr, "nico-sim: downloading base image: %s -> %s\n", imageURL, imagePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", fmt.Errorf("build base image request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download base image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download base image returned status %d", resp.StatusCode)
	}

	tmpPath := imagePath + ".download"
	file, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("create temp image file: %w", err)
	}
	progress := &downloadProgressWriter{lastPrint: time.Now()}
	written, copyErr := io.Copy(file, io.TeeReader(resp.Body, progress))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write base image: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close temp image file: %w", closeErr)
	}
	if err := os.Rename(tmpPath, imagePath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("finalize base image: %w", err)
	}
	if err := ValidateQCOW2(imagePath); err != nil {
		_ = os.Remove(imagePath)
		return "", fmt.Errorf("downloaded base image is not qcow2: %w", err)
	}
	fmt.Fprintf(os.Stderr, "nico-sim: downloaded base image: %d bytes\n", written)

	return imagePath, nil
}

// ValidateQCOW2 checks that the file at path begins with the qcow2 magic header.
func ValidateQCOW2(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	header := make([]byte, len(qcow2Magic))
	if _, err := io.ReadFull(file, header); err != nil {
		return err
	}
	if string(header) != qcow2Magic {
		return fmt.Errorf("unexpected qcow2 header %q", string(header))
	}
	return nil
}

type downloadProgressWriter struct {
	written   int64
	lastPrint time.Time
}

func (w *downloadProgressWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.written += int64(n)
	now := time.Now()
	if now.Sub(w.lastPrint) >= 5*time.Second {
		fmt.Fprintf(os.Stderr, "nico-sim: downloaded %.1f MiB...\n", float64(w.written)/(1024*1024))
		w.lastPrint = now
	}
	return n, nil
}
