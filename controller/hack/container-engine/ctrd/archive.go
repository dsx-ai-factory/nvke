// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxIndexBlob bounds the blobs read into memory: manifests and indexes, not layers.
const (
	maxIndexBlob = 4 << 20
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

func isIndex(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.index.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

func blobName(digest string) string { return "blobs/" + strings.Replace(digest, ":", "/", 1) }

// flattenArchive copies an OCI image archive, pointing every entry of its
// index.json that names a multi-platform index at the manifest of the platform
// instead. The index blob stays in the archive and nothing refers to it.
func flattenArchive(inPath, outPath, platform string) error {
	_, arch, _ := strings.Cut(platform, "/")
	arch, _, _ = strings.Cut(arch, "/")
	small, err := readSmallEntries(inPath)
	if err != nil {
		return err
	}
	var index map[string]json.RawMessage
	if err := json.Unmarshal(small["index.json"], &index); err != nil {
		return fmt.Errorf("reading index.json of the archive: %w", err)
	}
	var manifests []descriptor
	if err := json.Unmarshal(index["manifests"], &manifests); err != nil {
		return fmt.Errorf("reading index.json of the archive: %w", err)
	}
	for i, entry := range manifests {
		if !isIndex(entry.MediaType) {
			continue
		}
		var list struct {
			Manifests []descriptor `json:"manifests"`
		}
		if err := json.Unmarshal(small[blobName(entry.Digest)], &list); err != nil {
			return fmt.Errorf("reading the index %s of the archive: %w", entry.Digest, err)
		}
		for _, candidate := range list.Manifests {
			if candidate.Platform != nil && candidate.Platform.OS == "linux" && candidate.Platform.Architecture == arch && !isIndex(candidate.MediaType) {
				manifests[i].MediaType, manifests[i].Digest, manifests[i].Size = candidate.MediaType, candidate.Digest, candidate.Size
				break
			}
		}
	}
	if index["manifests"], err = json.Marshal(manifests); err != nil {
		return err
	}
	rewritten, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return copyArchive(inPath, outPath, rewritten)
}

// readSmallEntries reads index.json and the small blobs of the archive.
func readSmallEntries(path string) (map[string][]byte, error) {
	file, err := os.Open(path) //nolint:gosec // the archive is the caller's argument
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries := map[string][]byte{}
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeReg && (header.Name == "index.json" || header.Size <= maxIndexBlob) {
			if entries[header.Name], err = io.ReadAll(reader); err != nil {
				return nil, err
			}
		}
	}
}

// copyArchive writes the archive with index.json replaced.
func copyArchive(inPath, outPath string, index []byte) (retErr error) {
	in, err := os.Open(inPath) //nolint:gosec // the archive is the caller's argument
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(outPath) //nolint:gosec // the output file is the caller's argument
	if err != nil {
		return err
	}
	// The file may be made by root for the user whose umask is stricter, and who reads it: kind does.
	if err := out.Chmod(0o644); err != nil {
		out.Close()
		return err
	}
	defer func() {
		if err := out.Close(); retErr == nil {
			retErr = err
		}
	}()
	writer := tar.NewWriter(out)
	reader := tar.NewReader(in)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return writer.Close()
		}
		if err != nil {
			return err
		}
		var body io.Reader = reader
		if header.Name == "index.json" {
			header.Size = int64(len(index))
			body = bytes.NewReader(index)
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := io.Copy(writer, body); err != nil {
			return err
		}
	}
}
