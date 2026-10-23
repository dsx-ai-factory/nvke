// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
)

const (
	// Retry budget for artifact downloads. Nodes fetch these seconds after
	// their network comes up, so transient failures are expected.
	httpRetryAttempts     = 5
	httpRetryInitialDelay = 2 * time.Second
)

// BundleSourceType represents the type of bundle source
type BundleSourceType int

const (
	BundleSourceHTTP BundleSourceType = iota // HTTP/HTTPS/S3 bundle source
	BundleSourceOCI                          // OCI registry bundle source
	BundleSourceFile                         // Local file system bundle source
)

// BundleSource represents a parsed bundle source configuration
type BundleSource struct {
	Type        BundleSourceType
	URL         string // The URL (for HTTP/File sources) or registry URL (for OCI)
	OriginalURI string // The original URI as provided
}

// ParseBundleSourceURI parses a bundle source URI and determines its type.
// Supported schemes:
//   - oci://  - OCI registry (e.g., oci://registry.example.com/nvke/bundles)
//   - http:// or https:// - HTTP/S3 bundle server
//   - file:// - Local file system
//
// If no scheme is provided, defaults to HTTPS.
func ParseBundleSourceURI(uri string) (*BundleSource, error) {
	if uri == "" {
		return nil, fmt.Errorf("bundle source URI cannot be empty")
	}

	source := &BundleSource{
		OriginalURI: uri,
	}

	// Check for OCI scheme
	if strings.HasPrefix(uri, "oci://") {
		source.Type = BundleSourceOCI
		// Strip the oci:// prefix to get the registry URL
		source.URL = strings.TrimPrefix(uri, "oci://")
		// Remove trailing slash if present
		source.URL = strings.TrimSuffix(source.URL, "/")
		return source, nil
	}

	// Check for file scheme
	if strings.HasPrefix(uri, "file://") {
		source.Type = BundleSourceFile
		source.URL = uri
		return source, nil
	}

	// Check for HTTP/HTTPS scheme
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		source.Type = BundleSourceHTTP
		source.URL = uri
		return source, nil
	}

	// If no recognized scheme, treat as HTTP URL (add https://)
	source.Type = BundleSourceHTTP
	source.URL = "https://" + uri
	return source, nil
}

// IsOCI returns true if the bundle source is an OCI registry
func (bs *BundleSource) IsOCI() bool {
	return bs.Type == BundleSourceOCI
}

// IsHTTP returns true if the bundle source is HTTP/HTTPS
func (bs *BundleSource) IsHTTP() bool {
	return bs.Type == BundleSourceHTTP
}

// IsFile returns true if the bundle source is a local file system
func (bs *BundleSource) IsFile() bool {
	return bs.Type == BundleSourceFile
}

// String returns a human-readable representation of the bundle source type
func (t BundleSourceType) String() string {
	switch t {
	case BundleSourceHTTP:
		return "HTTP"
	case BundleSourceOCI:
		return "OCI"
	case BundleSourceFile:
		return "File"
	default:
		return "Unknown"
	}
}

const (
	// Relative paths under NKE_BUNDLE_SERVER_URL / --bundle-server-url.
	// Consumers host their own indexes at these paths, or pass --index-file-url.
	DefaultBundleIndexUbuntu22 = "nke_bundles/ubuntu_22_index.json"
	DefaultBundleIndexUbuntu24 = "nke_bundles/ubuntu_24_index.json"

	// ArchAarch64 is the arm64 label that uname reports.
	ArchAarch64 = "aarch64"
	// ArchArm64 is the arm64 label that Go reports.
	ArchArm64 = "arm64"
	// ArchX8664 is the amd64 label that uname reports.
	ArchX8664 = "x86_64"
	// ArchAmd64 is the amd64 label that Go reports.
	ArchAmd64 = "amd64"
	// ArchNA labels a bundle that is architecture-independent.
	ArchNA = "na"

	// bundleNameAgent and bundleNameAttestation install themselves detached,
	// because each one restarts the service running the install.
	bundleNameAgent       = "agent"
	bundleNameAttestation = "attestation"
)

var (
	ArmArchLabels = []string{ArchArm64, ArchAarch64, ArchNA}
	AmdArchLabels = []string{ArchAmd64, ArchX8664, ArchNA}
)

type Profile struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Bundles     []Bundle `json:"bundles"`
}

type Bundle struct {
	Name           string
	Version        *semver.Version
	InstallCmd     []string
	InstallFile    string
	LocalDirectory string
	Detached       bool
	UseStaging     bool
	Path           string
	URL            string
	OCIReference   string // OCI registry reference (e.g., registry.example.com/nvke/bundles/bundles-kubeadm:v1.30.0)
}

// Entry represents a single component entry
type Entry struct {
	Arch    string          `json:"arch"`
	Path    string          `json:"path"`
	Version *semver.Version `json:"version"`
}

// VersionString returns v as a string, or "" when v is nil, so a log line
// cannot panic on a bundle with no version.
func VersionString(v *semver.Version) string {
	if v == nil {
		return ""
	}
	return v.String()
}

// Component represents a component with multiple entries
type Component struct {
	Entries []Entry `json:"entries"`
}

// Index is the whole JSON structure, where the keys are component names
type Index map[string]Component

// Find will iterate over the index.json file and try to find the specfied bundle; name, version, arch
// currently relying on the bundles being in the same base URL as the index file
// returns the relative path to the bundle, caller is responsible for setting full path
func (idx Index) Find(name string, version *semver.Version, architecture string) (*Bundle, bool) {
	bundle := &Bundle{
		Name:    name,
		Version: version,
	}

	comp, ok := idx[bundle.Name]
	if !ok {
		return nil, false
	}

	found := false
	archLabels := AmdArchLabels
	if architecture == ArchArm64 || architecture == ArchAarch64 {
		archLabels = ArmArchLabels
	}

	slog.Info("searching for entries that match architecture", "name", bundle.Name, "version", VersionString(bundle.Version), "arch", architecture, "archLabels", archLabels)

	for _, p := range comp.Entries {
		slog.Info("comparing index entry to archlables", "IndexEntry", p, "ArchLabels", archLabels, "Architecture", architecture)
		if p.Version.Equal(bundle.Version) && slices.Contains(archLabels, p.Arch) {
			bundle.Path = p.Path
			found = true
			slog.Info("bundle found in index", "name", bundle.Name, "version", VersionString(bundle.Version), "arch", architecture, "path", bundle.Path)
			break
		}
	}

	return bundle, found
}

func GetMachineArchitecture() string {
	switch runtime.GOARCH {
	case ArchAmd64:
		return ArchX8664
	case ArchArm64:
		return ArchAarch64
	default:
		// Return the original Go arch name for erything else: "arm", "386", etc.
		return runtime.GOARCH
	}
}

// OSVersion represents supported Ubuntu versions
type OSVersion int

const (
	OSVersionUnsupported OSVersion = iota
	OSVersionUbuntu22
	OSVersionUbuntu24
)

var readFile = os.ReadFile

// GetOSVersion attempts to detect the Ubuntu version from /etc/os-release
func GetOSVersion() (OSVersion, error) {
	data, err := readFile("/etc/os-release")
	if err != nil {
		return 0, fmt.Errorf("failed to read /etc/os-release: %w", err)
	}

	content := string(data)

	if strings.Contains(content, "Ubuntu 24") {
		return OSVersionUbuntu24, nil
	}

	if strings.Contains(content, "Ubuntu 22") {
		return OSVersionUbuntu22, nil
	}

	return 0, fmt.Errorf("unsupported OS version: Ubuntu 22 or Ubuntu 24 required")
}

// GetBundleIndexPath returns the relative bundle index path for the host OS.
// Join with the configured bundle server base URL (see JoinBundleURL).
func GetBundleIndexPath() (string, error) {
	osVersion, err := GetOSVersion()
	if err != nil {
		return "", err
	}

	switch osVersion {
	case OSVersionUbuntu22:
		return DefaultBundleIndexUbuntu22, nil
	case OSVersionUbuntu24:
		return DefaultBundleIndexUbuntu24, nil
	case OSVersionUnsupported:
		fallthrough
	default:
		return "", fmt.Errorf("unsupported OS version")
	}
}

// JoinBundleURL joins a bundle server base URL with a relative path.
func JoinBundleURL(base, rel string) string {
	base = strings.TrimRight(base, "/") + "/"
	rel = strings.TrimLeft(rel, "/")
	return base + rel
}

func DropLastURLDir(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}

	p := strings.TrimSuffix(u.Path, "/")
	if p == "" || p == "/" {
		u.Path = "/"
		return u.String(), nil
	}

	dir := path.Dir(p)
	if dir != "/" {
		dir += "/"
	}
	u.Path = dir

	return u.String(), nil
}

// ConventionalBundleURL builds the URL a bundle is published at, without
// consulting the index.
//
// The index only maps (name, version, arch) to a path, and that path follows a
// fixed convention:
//
//	<directory of index.json>/<arch>/<name>_v<version>_<arch>.tgz
//
// Deriving the directory from the index URL rather than hardcoding it keeps
// this correct whichever distro directory the index lives in.
func ConventionalBundleURL(indexFileURL, name string, version *semver.Version, architecture string) string {
	if version == nil || indexFileURL == "" || name == "" || architecture == "" {
		return ""
	}
	base := indexFileURL
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[:i]
	}
	return fmt.Sprintf("%s/%s/%s_v%s_%s.tgz", base, architecture, name, version.String(), architecture)
}

// FetchBundleFromIndex resolves a bundle to a downloadable URL.
//
// It tries the conventional URL FIRST and falls back to the index only if that
// fails. The index is an extra round trip to a file whose whole job is to hold
// paths we can already predict, so requiring it lets an unreachable index block
// an artifact that is perfectly reachable. That is not hypothetical: a bundle
// server which serves artifacts but refuses index.json fails every CNI install
// with "unable to fetch index".
func FetchBundleFromIndex(ctx context.Context, bundleServerURL, indexFile, name string, version *semver.Version, architecture string) (*Bundle, error) {
	if directURL := ConventionalBundleURL(indexFile, name, version, architecture); directURL != "" {
		if err := URLExists(ctx, directURL); err == nil {
			slog.Info("resolved bundle directly, skipping the index", "url", directURL)
			return &Bundle{Name: name, Version: version, URL: directURL}, nil
		} else {
			slog.Info("direct bundle URL unavailable, falling back to the index",
				"url", directURL, "error", err)
		}
	}

	idx, err := FetchIndex(ctx, indexFile)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch index: %w", err)
	}

	bundle, found := idx.Find(name, version, architecture)
	if !found {
		slog.Error("bundle version not found in index", "bundle", bundle.Name, "version", VersionString(bundle.Version), "arch", architecture)
		return bundle, fmt.Errorf("%s bundle version %s for arch %s not found in index", bundle.Name, bundle.Version, architecture)
	}

	// JoinBundleURL, not concatenation: a bundle server URL without a trailing
	// slash otherwise yields http://hostnke_bundles/..., which fails at download
	// rather than here.
	bundle.URL = JoinBundleURL(bundleServerURL, bundle.Path)
	return bundle, nil
}

// URLExists reports whether a URL serves content. It uses HEAD, and falls back
// to GET for servers that do not implement HEAD.
func URLExists(ctx context.Context, url string) error {
	if resp, err := httpDoWithRetry(ctx, http.MethodHead, url); err == nil {
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusOK {
			return nil
		}
		if status != http.StatusMethodNotAllowed && status != http.StatusNotImplemented {
			return fmt.Errorf("bad response: %d", status)
		}
	}
	resp, err := httpDoWithRetry(ctx, http.MethodGet, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad response: %s", resp.Status)
	}
	return nil
}

// httpDoWithRetry issues a request, retrying transport errors and 5xx with
// backoff. Artifact downloads run on freshly provisioned nodes whose networking
// has just come up, so a single transient failure should not fail an install.
// 4xx is returned immediately: it will not become a 200 by waiting.
func httpDoWithRetry(ctx context.Context, method, url string) (*http.Response, error) {
	var lastErr error
	delay := httpRetryInitialDelay
	for attempt := 1; attempt <= httpRetryAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode < 500:
			return resp, nil
		default:
			lastErr = fmt.Errorf("bad response: %s", resp.Status)
			resp.Body.Close()
		}
		if attempt == httpRetryAttempts {
			break
		}
		slog.Warn("retrying request", "method", method, "url", url,
			"attempt", attempt, "error", lastErr, "sleep", delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return nil, lastErr
}

func InstallBundle(b *Bundle, runner CommandRunner, source *BundleSource) (string, error) {
	slog.Info("begin install process for bundle", "bundle", b)
	path, err := DownloadAndExtractArchive(b, source)
	if err != nil {
		slog.Error("failed to download and extract archive", "error", err)
		return "", err
	}

	localDirectory := path
	if _, err := os.Stat(localDirectory); errors.Is(err, os.ErrNotExist) {
		slog.Error("failed to find bundle directory", "bundleDir", localDirectory, "error", err)
		return "", err
	}

	slog.Info("attempting install command in directory", "directory", localDirectory)
	if b.InstallFile != "" {
		_, _ = runner.Run("chmod", []string{"+x", b.InstallFile}, &CommandOptions{Dir: localDirectory})
	} else {
		// Default to install.sh if nothing else provided; eventually we should always use install.sh for all bundles
		// to simplify this, even if install.sh is a wrapper to something else like a python script.
		b.InstallFile = "install.sh"
	}

	// If it's a .sh or .py executable (eg install.sh or install.py) make sure
	// it's executable, this will cover both syn and async calls
	if len(b.InstallCmd) > 0 && (strings.Contains(b.InstallCmd[0], ".sh") || strings.Contains(b.InstallCmd[0], ".py")) {
		out, chmodErr := runner.Run("chmod", []string{"+x", b.InstallCmd[0]}, &CommandOptions{Dir: localDirectory})
		if chmodErr != nil {
			log.Printf("chmod failed: %s", string(out))
			return string(out), chmodErr
		}
	}

	runDetached := (b.Name == bundleNameAgent || b.Name == bundleNameAttestation)

	var installErr error
	output := ""
	if runDetached {
		installErr = runner.RunDetached(b.InstallCmd[0], b.InstallCmd[1:], &CommandOptions{Dir: localDirectory})
	} else {
		var out []byte
		log.Printf("Issuing runner call with the following... installCmd:%s, cmdArgs:%s, localDirectory:%s", b.InstallCmd[0], b.InstallCmd[1:], localDirectory)
		out, installErr = runner.Run(b.InstallCmd[0], b.InstallCmd[1:], &CommandOptions{Dir: localDirectory})
		if installErr != nil {
			log.Printf("install of bundle failed: %s", installErr.Error())
			log.Printf("output was: %s", string(out))
			log.Printf("bundle details:  %+v", b)
			output = string(out)
		}
	}
	return output, installErr
}

// DownloadAndExtractArchive downloads the specified bundle and extracts it.
// If source is OCI, pulls from OCI registry; otherwise uses HTTP/S3.
func DownloadAndExtractArchive(b *Bundle, source *BundleSource) (string, error) {
	if source != nil && source.IsOCI() {
		arch := GetArchString()
		osVersion, err := GetOSVersionString()
		if err != nil {
			slog.Warn("failed to get OS version for OCI, falling back to HTTP", "error", err)
			return downloadAndExtractFromHTTP(b)
		}
		return downloadAndExtractFromOCI(context.Background(), b, source.URL, arch, osVersion)
	}
	return downloadAndExtractFromHTTP(b)
}

// downloadAndExtractFromHTTP downloads the bundle from HTTP/S3 and extracts it
func downloadAndExtractFromHTTP(b *Bundle) (string, error) {
	outputDir, err := os.MkdirTemp("", fmt.Sprintf("%s_%s-", b.Name, b.Version))
	if err != nil {
		return "", fmt.Errorf("failed to create tmp dir: %w", err)
	}

	slog.Info("attempt download of bundle from HTTP", "URL", b.URL)
	resp, err := httpDoWithRetry(context.Background(), http.MethodGet, b.URL)
	if err != nil {
		return outputDir, fmt.Errorf("failed to download tgz: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return outputDir, fmt.Errorf("bad response: %s", resp.Status)
	}

	// Write the downloaded archive to a temporary file
	tmpFile := filepath.Join(outputDir, "bundle.tgz")
	out, err := os.Create(tmpFile)
	if err != nil {
		return outputDir, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile) // Clean up the archive file
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return outputDir, fmt.Errorf("failed to write temp archive: %w", err)
	}
	out.Close()
	// Extract the archive contents, stripping the top-level directory
	cmd := exec.Command("tar", "-xzf", tmpFile, "--strip-components=1", "-C", outputDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return outputDir, fmt.Errorf("tar extraction failed: %w", err)
	}

	return outputDir, nil
}

// downloadAndExtractFromOCI downloads the bundle from OCI registry and extracts it.
func downloadAndExtractFromOCI(ctx context.Context, b *Bundle, ociRegistryURL, arch, osVersion string) (string, error) {
	slog.Info("pulling bundle from OCI registry",
		"bundle", b.Name,
		"version", VersionString(b.Version),
		"registry", ociRegistryURL,
		"arch", arch,
		"osVersion", osVersion)

	bundleDir, err := PullBundleFromOCI(ctx, ociRegistryURL, b.Name, b.Version.String(), arch, osVersion)
	if err != nil {
		slog.Error("failed to pull bundle from OCI registry",
			"bundle", b.Name,
			"version", VersionString(b.Version),
			"error", err)
		return "", fmt.Errorf("failed to pull bundle %s from OCI registry: %w", b.Name, err)
	}

	slog.Info("successfully pulled bundle from OCI registry", "bundle", b.Name, "bundleDir", bundleDir)

	// Extract the OCI bundle
	extractedDir, err := ExtractOCIBundle(bundleDir)
	if err != nil {
		slog.Error("failed to extract OCI bundle",
			"bundle", b.Name,
			"error", err)
		os.RemoveAll(bundleDir)
		return "", fmt.Errorf("failed to extract OCI bundle %s: %w", b.Name, err)
	}

	// Store the OCI reference in the bundle for logging/tracking
	b.OCIReference = BuildBundleReference(ociRegistryURL, b.Name, b.Version.String())

	return extractedDir, nil
}

// FetchBundleFromOCIRegistry fetches a bundle directly from the OCI registry
// This is an alternative to FetchBundleFromIndex when using OCI registries
func FetchBundleFromOCIRegistry(ctx context.Context, ociRegistryURL, name string, version *semver.Version, arch, osVersion string) (*Bundle, error) {
	bundle := &Bundle{
		Name:         name,
		Version:      version,
		OCIReference: BuildBundleReference(ociRegistryURL, name, version.String()),
	}

	slog.Info("preparing bundle from OCI registry",
		"name", name,
		"version", VersionString(version),
		"ociReference", bundle.OCIReference)

	return bundle, nil
}

// FetchIndex will fetch the index file from the specified bundle URL.
func FetchIndex(ctx context.Context, indexFileURL string) (Index, error) {
	var index Index
	slog.Info("fetching index file", "INDEX_FILE_URL", indexFileURL)
	if err := FetchJSON(ctx, indexFileURL, &index); err != nil {
		return nil, fmt.Errorf("unable to fetch index: %w", err)
	}
	return index, nil
}

func ExtractVersion(filename string) (string, error) {
	re := regexp.MustCompile(`_(v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9]+-g[0-9a-f]+(?:-dirty)?)?)\.tgz$`)
	matches := re.FindStringSubmatch(filename)
	if len(matches) < 2 {
		return "", fmt.Errorf("no version found in filename: %s", filename)
	}
	return matches[1], nil
}
