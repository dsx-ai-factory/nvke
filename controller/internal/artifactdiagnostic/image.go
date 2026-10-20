// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

type descriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
}

// inspectImage verifies the supplied single-native-manifest OCI archive without executing it.
func inspectImage(ctx context.Context, file string) (string, string, string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", "", "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	metadata := map[string][]byte{}
	seen := map[string]bool{}
	var total int64
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return "", "", "", err
		}
		h, e := tr.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return "", "", "", e
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir && (name == "blobs/" || name == "blobs/sha256/") {
			continue
		}
		digest := strings.TrimPrefix(name, "blobs/sha256/")
		if count > 10000 || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > 4<<30-total || seen[name] || (name != "index.json" && name != "oci-layout" && (!strings.HasPrefix(name, "blobs/sha256/") || !validDigest("sha256:"+digest))) {
			return "", "", "", errors.New("unsupported OCI archive entry")
		}
		total += h.Size
		seen[name] = true
		hash := sha256.New()
		var raw []byte
		if h.Size <= 8<<20 {
			raw, e = io.ReadAll(io.TeeReader(tr, hash))
		} else {
			_, e = io.CopyN(hash, tr, h.Size)
		}
		if e != nil {
			return "", "", "", e
		}
		if strings.HasPrefix(name, "blobs/") && hex.EncodeToString(hash.Sum(nil)) != digest {
			return "", "", "", errors.New("OCI blob digest mismatch")
		}
		if raw != nil {
			metadata[name] = raw
		}
	}
	var marker struct {
		Version string `json:"imageLayoutVersion"`
	}
	if json.Unmarshal(metadata["oci-layout"], &marker) != nil || marker.Version != "1.0.0" {
		return "", "", "", errors.New("invalid OCI layout")
	}
	var idx struct {
		Manifests []descriptor `json:"manifests"`
	}
	if json.Unmarshal(metadata["index.json"], &idx) != nil || len(idx.Manifests) != 1 || !validDigest(idx.Manifests[0].Digest) {
		return "", "", "", errors.New("requires one native image manifest")
	}
	manifest := idx.Manifests[0].Digest
	var image struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if json.Unmarshal(metadata["blobs/sha256/"+strings.TrimPrefix(manifest, "sha256:")], &image) != nil || !validDigest(image.Config.Digest) {
		return "", "", "", errors.New("invalid native manifest")
	}
	for _, layer := range image.Layers {
		if !validDigest(layer.Digest) || !seen["blobs/sha256/"+strings.TrimPrefix(layer.Digest, "sha256:")] {
			return "", "", "", errors.New("missing image layer")
		}
	}
	config := image.Config.Digest
	var cfg struct {
		OS   string `json:"os"`
		Arch string `json:"architecture"`
	}
	if json.Unmarshal(metadata["blobs/sha256/"+strings.TrimPrefix(config, "sha256:")], &cfg) != nil || cfg.OS != nativeOS || cfg.Arch != nativeArch {
		return "", "", "", errors.New("image is not native Linux AMD64")
	}
	sum := sha256.Sum256(metadata["index.json"])
	return "sha256:" + hex.EncodeToString(sum[:]), manifest, config, nil
}
