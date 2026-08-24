// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// NVIDIABundleMediaType is the media type for NVIDIA bundle artifacts
	NVIDIABundleMediaType = "application/vnd.nvidia.bundle.v1+tgz"

	// NGC OAuth token username used for authentication
	NGCOAuthUsername = "$oauthtoken"
)

// OCIConfig holds configuration for OCI registry operations
type OCIConfig struct {
	// RegistryURL is the base OCI registry URL (e.g., "registry.example.com/nke/bundles")
	RegistryURL string
}

// BuildBundleReference constructs an OCI reference for a bundle
// Format: {registry-url}/bundles-{name}:{version}
func BuildBundleReference(registryURL, bundleName, version string) string {
	return fmt.Sprintf("%s/bundles-%s:%s", registryURL, bundleName, version)
}

// BuildCollectionReference constructs an OCI reference for a collection
// Format: {registry-url}/bundles-collections:{version} or
//
//	{registry-url}/bundles-collections-{variant}:{version}
func BuildCollectionReference(registryURL, collectionName, version string) string {
	if collectionName == "" || collectionName == "default" {
		return fmt.Sprintf("%s/bundles-collections:%s", registryURL, version)
	}
	return fmt.Sprintf("%s/bundles-collections-%s:%s", registryURL, collectionName, version)
}

// getCredentials returns authentication credentials from environment
// Returns nil if no credentials are available (anonymous access)
func getCredentials() *auth.Client {
	ngcAPIKey := os.Getenv("NGC_API_KEY")
	if ngcAPIKey == "" {
		slog.Debug("NGC_API_KEY not set, using anonymous access")
		return nil
	}

	return &auth.Client{
		Client: retry.DefaultClient,
		Credential: auth.StaticCredential("nvcr.io", auth.Credential{
			Username: NGCOAuthUsername,
			Password: ngcAPIKey,
		}),
	}
}

// setupRepository creates and configures an OCI repository client
func setupRepository(reference string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(reference)
	if err != nil {
		return nil, fmt.Errorf("failed to create repository: %w", err)
	}

	// Set up authentication if available
	if client := getCredentials(); client != nil {
		repo.Client = client
	}

	return repo, nil
}

// findPlatformManifest finds the manifest matching the specified platform in an image index
func findPlatformManifest(ctx context.Context, repo *remote.Repository, indexDesc ocispec.Descriptor, arch, osVersion string) (*ocispec.Descriptor, error) {
	// Fetch the index manifest
	rc, err := repo.Fetch(ctx, indexDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch index: %w", err)
	}
	defer rc.Close()

	indexData, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read index: %w", err)
	}

	// Parse as OCI index
	var index ocispec.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return nil, fmt.Errorf("failed to parse index: %w", err)
	}

	slog.Debug("searching for platform in manifest index",
		"arch", arch,
		"osVersion", osVersion,
		"manifests", len(index.Manifests))

	// Find matching platform
	for _, manifest := range index.Manifests {
		if manifest.Platform == nil {
			continue
		}

		platform := manifest.Platform
		slog.Debug("checking manifest platform",
			"manifestArch", platform.Architecture,
			"manifestOS", platform.OS,
			"manifestOSVersion", platform.OSVersion)

		// Match architecture and OS version
		if platform.Architecture == arch && platform.OSVersion == osVersion {
			slog.Info("found matching platform manifest",
				"arch", arch,
				"osVersion", osVersion,
				"digest", manifest.Digest.String())
			return &manifest, nil
		}
	}

	return nil, fmt.Errorf("no manifest found for platform linux/%s with os.version %s", arch, osVersion)
}

// PullBundleFromOCI pulls a bundle from an OCI registry
// It handles multi-platform manifests and selects the correct platform variant
func PullBundleFromOCI(ctx context.Context, registryURL, bundleName, version, arch, osVersion string) (string, error) {
	reference := BuildBundleReference(registryURL, bundleName, version)
	slog.Info("pulling bundle from OCI registry",
		"reference", reference,
		"arch", arch,
		"osVersion", osVersion)

	return pullArtifact(ctx, reference, arch, osVersion)
}

// PullCollectionFromOCI pulls a collection from an OCI registry
func PullCollectionFromOCI(ctx context.Context, registryURL, collectionName, version, arch, osVersion string) (string, error) {
	reference := BuildCollectionReference(registryURL, collectionName, version)
	slog.Info("pulling collection from OCI registry",
		"reference", reference,
		"arch", arch,
		"osVersion", osVersion)

	return pullArtifact(ctx, reference, arch, osVersion)
}

// pullArtifact is the core function for pulling OCI artifacts with platform selection
func pullArtifact(ctx context.Context, reference, arch, osVersion string) (string, error) {
	// Create output directory
	outputDir, err := os.MkdirTemp("", "oci-bundle-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Set up repository
	repo, err := setupRepository(reference)
	if err != nil {
		os.RemoveAll(outputDir)
		return "", err
	}

	// Resolve the reference to get the descriptor
	desc, err := repo.Resolve(ctx, reference)
	if err != nil {
		os.RemoveAll(outputDir)
		return "", fmt.Errorf("failed to resolve reference %s: %w", reference, err)
	}

	slog.Debug("resolved OCI reference",
		"reference", reference,
		"mediaType", desc.MediaType,
		"digest", desc.Digest.String())

	// Check if this is an image index (multi-platform)
	targetDesc := desc
	if desc.MediaType == ocispec.MediaTypeImageIndex || desc.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		slog.Debug("detected multi-platform manifest, selecting platform")
		platformDesc, err := findPlatformManifest(ctx, repo, desc, arch, osVersion)
		if err != nil {
			os.RemoveAll(outputDir)
			return "", err
		}
		targetDesc = *platformDesc
	}

	// Create file store for output
	fs, err := file.New(outputDir)
	if err != nil {
		os.RemoveAll(outputDir)
		return "", fmt.Errorf("failed to create file store: %w", err)
	}
	defer fs.Close()

	// Copy the artifact to the file store
	copyOpts := oras.DefaultCopyOptions
	_, err = oras.Copy(ctx, repo, targetDesc.Digest.String(), fs, "", copyOpts)
	if err != nil {
		os.RemoveAll(outputDir)
		return "", fmt.Errorf("failed to pull artifact: %w", err)
	}

	slog.Info("successfully pulled OCI artifact",
		"reference", reference,
		"outputDir", outputDir)

	return outputDir, nil
}

// ExtractOCIBundle extracts a pulled OCI bundle tarball to a directory
// Returns the path to the extracted bundle
func ExtractOCIBundle(bundleDir string) (string, error) {
	// Find the tarball in the bundle directory
	var tarballPath string
	err := filepath.Walk(bundleDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && (strings.HasSuffix(path, ".tgz") || strings.HasSuffix(path, ".tar.gz")) {
			tarballPath = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to find tarball: %w", err)
	}

	if tarballPath == "" {
		// If no tarball found, the content may already be extracted
		// Check if there are files in the directory
		entries, err := os.ReadDir(bundleDir)
		if err != nil {
			return "", fmt.Errorf("failed to read bundle dir: %w", err)
		}
		if len(entries) > 0 {
			slog.Debug("no tarball found, content may already be extracted", "dir", bundleDir)
			return bundleDir, nil
		}
		return "", fmt.Errorf("no tarball or content found in bundle directory")
	}

	// Create extraction directory
	extractDir, err := os.MkdirTemp("", "bundle-extracted-")
	if err != nil {
		return "", fmt.Errorf("failed to create extraction dir: %w", err)
	}

	// Extract using tar command (consistent with existing S3 implementation)
	runner := &ExecRunner{}
	_, err = runner.Run("tar", []string{"-xzf", tarballPath, "--strip-components=1", "-C", extractDir}, nil)
	if err != nil {
		os.RemoveAll(extractDir)
		return "", fmt.Errorf("tar extraction failed: %w", err)
	}

	// Clean up the original bundle directory
	os.RemoveAll(bundleDir)

	slog.Info("extracted OCI bundle", "extractDir", extractDir)
	return extractDir, nil
}

// GetOSVersionString returns the OS version string for OCI platform matching
func GetOSVersionString() (string, error) {
	osVersion, err := GetOSVersion()
	if err != nil {
		return "", err
	}

	switch osVersion {
	case OSVersionUbuntu22:
		return "22.04", nil
	case OSVersionUbuntu24:
		return "24.04", nil
	case OSVersionUnsupported:
		return "", fmt.Errorf("unsupported OS version for OCI")
	default:
		return "", fmt.Errorf("unsupported OS version for OCI")
	}
}

// GetArchString returns the architecture string for OCI platform matching
// Converts Go runtime arch to OCI-compatible arch names
func GetArchString() string {
	arch := GetMachineArchitecture()
	switch arch {
	case ArchX8664:
		return ArchAmd64
	case ArchAarch64:
		return ArchArm64
	default:
		return arch
	}
}
