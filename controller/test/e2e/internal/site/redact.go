// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const (
	// minRedactable is the shortest value worth searching for. A journal is
	// full of short strings, so a three-byte token would match unrelated text
	// and cost more in readability than it buys, and a credential that short
	// is not protecting anything. Shorter values are dropped rather than
	// half-applied.
	minRedactable = 8

	redactedMarker = "[redacted by the e2e suite]"
)

// redactArchive rewrites a gzipped tar, replacing every occurrence of each
// secret in every member's contents.
//
// The archive is diagnostics collected off a node the run placed credentials
// on, and it is uploaded as a CI artifact, so it leaves the trust boundary
// the credentials were scoped to. The suite knows the exact values it wrote,
// which is what makes substitution the right mechanism rather than looking
// for things shaped like secrets.
//
// Both the literal value and its base64 encoding are replaced, because
// anything that captured a Kubernetes Secret -- kubectl get secret -o yaml,
// or a rendered manifest -- carries the value encoded rather than plain. That
// covers a value encoded on its own and not one embedded inside a larger
// base64 blob, where the alignment differs; a whole kubeconfig encoded in one
// field would survive this.
func redactArchive(data []byte, secrets []string) ([]byte, error) {
	needles := redactions(secrets)
	if len(needles) == 0 {
		return data, nil
	}

	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read the diagnostics archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the diagnostics archive: %w", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s out of the diagnostics archive: %w", header.Name, err)
		}
		for _, needle := range needles {
			content = bytes.ReplaceAll(content, needle, []byte(redactedMarker))
		}
		// The marker is a different length from what it replaced, so the
		// header has to be rewritten rather than copied.
		header.Size = int64(len(content))
		if err := tw.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write %s to the redacted archive: %w", header.Name, err)
		}
		if _, err := tw.Write(content); err != nil {
			return nil, fmt.Errorf("write %s to the redacted archive: %w", header.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close the redacted archive: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close the redacted archive: %w", err)
	}
	return out.Bytes(), nil
}

// redactions is the byte sequences to remove: every secret long enough to
// search for, plus its base64 encoding, deduplicated.
func redactions(secrets []string) [][]byte {
	seen := make(map[string]bool, len(secrets)*2)
	var needles [][]byte
	add := func(value string) {
		if len(value) < minRedactable || seen[value] {
			return
		}
		seen[value] = true
		needles = append(needles, []byte(value))
	}
	for _, secret := range secrets {
		if len(secret) < minRedactable {
			continue
		}
		add(secret)
		add(base64.StdEncoding.EncodeToString([]byte(secret)))
	}
	return needles
}
