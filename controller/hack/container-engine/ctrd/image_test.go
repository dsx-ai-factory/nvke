// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeRef(t *testing.T) {
	const digest = "@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
	tests := map[string]string{
		"registry:2":                           "docker.io/library/registry:2",
		"ubuntu":                               "docker.io/library/ubuntu:latest",
		"kindest/node:v1.37.0" + digest:        "docker.io/kindest/node:v1.37.0" + digest,
		"kindest/node" + digest:                "docker.io/kindest/node" + digest,
		"cwe-capd-kindest-node:local":          "docker.io/library/cwe-capd-kindest-node:local",
		"docker.local/cwe:tilt":                "docker.local/cwe:tilt",
		"quay.io/frrouting/frr:9.1.0":          "quay.io/frrouting/frr:9.1.0",
		"localhost:5000/team/app":              "localhost:5000/team/app:latest",
		"localhost/app:1":                      "localhost/app:1",
		"index.docker.io/library/alpine:3":     "docker.io/library/alpine:3",
		"docker.io/alpine":                     "docker.io/library/alpine:latest",
		"registry.k8s.io/pause:3.9":            "registry.k8s.io/pause:3.9",
		"myregistry.example.com:5443/a/b/c:v1": "myregistry.example.com:5443/a/b/c:v1",
	}
	for ref, want := range tests {
		t.Run(ref, func(t *testing.T) { assert.Equal(t, want, normalizeRef(ref)) })
	}
}

// archive builds an OCI image archive: its entries, in order.
func archive(t *testing.T, entries map[string]string, order []string) string {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, name := range order {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Size: int64(len(entries[name])), Mode: 0o644, Typeflag: tar.TypeReg}))
		_, err := writer.Write([]byte(entries[name]))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	path := filepath.Join(t.TempDir(), "in.tar")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	return path
}

func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	entries := map[string]string{}
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		entries[header.Name] = string(data)
	}
}

func TestFlattenArchive(t *testing.T) {
	const (
		indexDigest = "sha256:1111"
		armDigest   = "sha256:2222"
		amdDigest   = "sha256:3333"
		single      = "sha256:4444"
	)
	list := `{"manifests":[
	  {"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + amdDigest + `","size":10,"platform":{"os":"linux","architecture":"amd64"}},
	  {"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:5555","size":11,"platform":{"os":"unknown","architecture":"unknown"}},
	  {"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + armDigest + `","size":12,"platform":{"os":"linux","architecture":"arm64"}}]}`
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
	  {"mediaType":"application/vnd.oci.image.index.v1+json","digest":"` + indexDigest + `","size":99,"annotations":{"io.containerd.image.name":"docker.io/library/a:1"}},
	  {"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + single + `","size":7,"annotations":{"io.containerd.image.name":"docker.local/b:2"}}]}`
	const bigLayer = "blobs/sha256/9999"
	entries := map[string]string{
		"oci-layout":        `{"imageLayoutVersion":"1.0.0"}`,
		"index.json":        index,
		"blobs/sha256/1111": list,
		"blobs/sha256/2222": `{"manifest":"arm"}`,
		"blobs/sha256/4444": `{"manifest":"single"}`,
		bigLayer:            strings.Repeat("l", maxIndexBlob+1),
	}
	order := []string{"oci-layout", "index.json", "blobs/sha256/1111", "blobs/sha256/2222", "blobs/sha256/4444", bigLayer}
	in := archive(t, entries, order)
	out := filepath.Join(t.TempDir(), "out.tar")

	require.NoError(t, flattenArchive(in, out, "linux/arm64"))

	got := readArchive(t, out)
	assert.Len(t, got, len(entries), "every entry is copied")
	for name, want := range entries {
		switch name {
		case "index.json":
		case bigLayer:
			assert.Len(t, got[name], len(want), name)
		default:
			assert.Equal(t, want, got[name], name)
		}
	}
	var rewritten struct {
		SchemaVersion int `json:"schemaVersion"`
		Manifests     []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	require.NoError(t, json.Unmarshal([]byte(got["index.json"]), &rewritten))
	require.Len(t, rewritten.Manifests, 2)
	assert.Equal(t, 2, rewritten.SchemaVersion)
	assert.Equal(t, "application/vnd.oci.image.manifest.v1+json", rewritten.Manifests[0].MediaType)
	assert.Equal(t, armDigest, rewritten.Manifests[0].Digest, "the index entry names the manifest of the platform")
	assert.EqualValues(t, 12, rewritten.Manifests[0].Size)
	assert.Equal(t, "docker.io/library/a:1", rewritten.Manifests[0].Annotations["io.containerd.image.name"], "the image name stays")
	assert.Equal(t, single, rewritten.Manifests[1].Digest, "a single manifest is left alone")
}

func TestFlattenArchiveRejectsAnArchiveWithoutAnIndex(t *testing.T) {
	in := archive(t, map[string]string{"oci-layout": "{}"}, []string{"oci-layout"})
	require.Error(t, flattenArchive(in, filepath.Join(t.TempDir(), "out.tar"), "linux/arm64"))
}
