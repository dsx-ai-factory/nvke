// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package etcdkey persists the etcd encryption key of a bootstrapped site
// cluster, so the key outlives the bootstrap cluster that generated it.
//
// CWE keeps a cluster's key in a Secret on the cluster it runs on. During a
// bootstrap that is the ephemeral Kind cluster, which is deleted when the
// command finishes; the nodes keep the key on disk, but nothing that could
// drive them later does. This package writes the Secret out as a manifest a
// controller installed afterwards can be given.
package etcdkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// secretPrefix and FieldActive name the Secret and the field CWE reads a
	// cluster's active key from. They must match EtcdEncryptionKeySecretPrefix
	// and the active field name in controller/internal/constants/constants.go,
	// which this module cannot import. The bootstrap e2e reads the real Secret
	// by this name and fails if they drift.
	secretPrefix = "etcd-encryption-key-"
	FieldActive  = "active"

	// ClusterNameAnnotation records the site spec cluster the key belongs to.
	// The Secret is named for the cluster's UUID, which the operator never
	// sees; this is how a file on disk is matched back to a cluster.
	ClusterNameAnnotation = "nke.nvidia.com/cluster-name"
)

// ErrExists is returned by Write when path already holds a file.
var ErrExists = errors.New("etcdkey: file already exists")

// SecretName is the name CWE gives the key Secret of cluster clusterID.
func SecretName(clusterID uuid.UUID) string {
	return secretPrefix + clusterID.String()
}

// Manifest renders the key as a Secret manifest.
//
// No namespace: the controller that will eventually hold this key is
// installed after the bootstrap, into a namespace chosen then. The manifest
// is applied with -n at that point, and a namespace baked in here would have
// to match it.
func Manifest(clusterID uuid.UUID, clusterName string, data map[string][]byte) ([]byte, error) {
	if len(data[FieldActive]) == 0 {
		return nil, fmt.Errorf("etcdkey: key data for %s has no %q field", SecretName(clusterID), FieldActive)
	}
	secret := corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        SecretName(clusterID),
			Annotations: map[string]string{ClusterNameAnnotation: clusterName},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	out, err := json.MarshalIndent(secret, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("etcdkey: encoding %s: %w", secret.Name, err)
	}
	return append(out, '\n'), nil
}

// Write persists manifest at path and confirms what landed on disk is what
// was given.
//
// The file is created 0600 under a new name, synced, then linked into place;
// the link fails if path exists, so an earlier file is never overwritten and
// a symlink planted at path is never followed. The directory is synced too,
// and the file is read back and compared: what this protects is about to be
// the only copy outside the cluster, and a write that has not reached the
// disk is not a copy.
func Write(path string, manifest []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("etcdkey: creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("etcdkey: creating a file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := writeSynced(tmp, manifest); err != nil {
		return fmt.Errorf("etcdkey: writing %s: %w", tmpPath, err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		return fmt.Errorf("etcdkey: placing %s: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("etcdkey: syncing %s: %w", dir, err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("etcdkey: reading back %s: %w", path, err)
	}
	if !bytes.Equal(got, manifest) {
		return fmt.Errorf("etcdkey: %s does not contain what was written (%d bytes on disk, %d written)",
			path, len(got), len(manifest))
	}
	return nil
}

func writeSynced(f *os.File, b []byte) error {
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Fingerprint returns the SHA-256 of the active key, hex-encoded. It is what
// a log line or a message may carry: enough to tell two keys apart, and
// nothing that decrypts anything.
func Fingerprint(data map[string][]byte) string {
	sum := sha256.Sum256(data[FieldActive])
	return hex.EncodeToString(sum[:])
}
