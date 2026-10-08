// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	semver "github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
)

func TestIndexFind(t *testing.T) {
	index := Index{
		"kubeadm": Component{
			Entries: []Entry{
				{Arch: "amd64", Version: semver.MustParse("v1.28.0"), Path: "bundles/kubeadm_v1.28.0_amd64.tgz"},
				{Arch: "arm64", Version: semver.MustParse("v1.28.0"), Path: "bundles/kubeadm_v1.28.0_arm64.tgz"},
				{Arch: "amd64", Version: semver.MustParse("v1.29.0"), Path: "bundles/kubeadm_v1.29.0_amd64.tgz"},
			},
		},
		"agent": Component{
			Entries: []Entry{
				{Arch: "na", Version: semver.MustParse("v2.0.0"), Path: "bundles/agent_v2.0.0.tgz"},
			},
		},
	}

	t.Run("find existing bundle for amd64", func(t *testing.T) {
		bundle, found := index.Find("kubeadm", semver.MustParse("v1.28.0"), "amd64")

		assert.True(t, found)
		assert.NotNil(t, bundle)
		assert.Equal(t, "kubeadm", bundle.Name)
		assert.Equal(t, semver.MustParse("v1.28.0"), bundle.Version)
		assert.Equal(t, "bundles/kubeadm_v1.28.0_amd64.tgz", bundle.Path)
		assert.False(t, bundle.UseStaging)
	})

	t.Run("find existing bundle for arm64", func(t *testing.T) {
		bundle, found := index.Find("kubeadm", semver.MustParse("v1.28.0"), "arm64")

		assert.True(t, found)
		assert.NotNil(t, bundle)
		assert.Equal(t, "kubeadm", bundle.Name)
		assert.Equal(t, semver.MustParse("v1.28.0"), bundle.Version)
		assert.Equal(t, "bundles/kubeadm_v1.28.0_arm64.tgz", bundle.Path)
	})

	t.Run("bundle not found - wrong version", func(t *testing.T) {
		_, found := index.Find("kubeadm", semver.MustParse("v1.27.0"), "amd64")
		assert.False(t, found)
	})

	t.Run("bundle not found - component doesn't exist", func(t *testing.T) {
		_, found := index.Find("nonexistent", semver.MustParse("v1.0.0"), "amd64")
		assert.False(t, found)
	})
}

func TestParseIndex(t *testing.T) {
	const indexJSON = `
{
  "agent": {
    "entries": [
      {
        "path": "nke_bundles/Ubuntu_24.04/aarch64/agent_v0.1.0_arm64.tgz",
        "version": "0.1.0"
      },
      {
        "path": "nke_bundles/Ubuntu_24.04/aarch64/agent_v0.0.41-patch1_arm64.tgz",
        "version": "v0.0.41"
      }
	]
  }
}
	`

	var index Index
	err := json.Unmarshal([]byte(indexJSON), &index)
	assert.NoError(t, err)

	assert.Equal(t, index["agent"].Entries[0].Version.String(), "0.1.0")
	assert.Equal(t, index["agent"].Entries[1].Version.String(), "0.0.41")
}

func TestGetOSVersion(t *testing.T) {
	// Save original readFile function and restore after test
	originalReadFile := readFile
	defer func() { readFile = originalReadFile }()

	t.Run("Ubuntu 22.04", func(t *testing.T) {
		// Mock os-release content for Ubuntu 22.04
		osReleaseContent := `PRETTY_NAME="Ubuntu 22.04.5 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
VERSION="22.04.5 LTS (Jammy Jellyfish)"
VERSION_CODENAME=jammy
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
SUPPORT_URL="https://help.ubuntu.com/"
BUG_REPORT_URL="https://bugs.launchpad.net/ubuntu/"
PRIVACY_POLICY_URL="https://www.ubuntu.com/legal/terms-and-policies/privacy-policy"
UBUNTU_CODENAME=jammy`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		version, err := GetOSVersion()
		assert.NoError(t, err)
		assert.Equal(t, OSVersionUbuntu22, version)
	})

	t.Run("Ubuntu 24.04", func(t *testing.T) {
		// Mock os-release content for Ubuntu 24.04
		osReleaseContent := `PRETTY_NAME="Ubuntu 24.04 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
SUPPORT_URL="https://help.ubuntu.com/"
BUG_REPORT_URL="https://bugs.launchpad.net/ubuntu/"
PRIVACY_POLICY_URL="https://www.ubuntu.com/legal/terms-and-policies/privacy-policy"
UBUNTU_CODENAME=noble`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		version, err := GetOSVersion()
		assert.NoError(t, err)
		assert.Equal(t, OSVersionUbuntu24, version)
	})

	t.Run("Unsupported Ubuntu version", func(t *testing.T) {
		// Mock os-release content for Ubuntu 20.04 (unsupported)
		osReleaseContent := `PRETTY_NAME="Ubuntu 20.04.6 LTS"
NAME="Ubuntu"
VERSION_ID="20.04"
VERSION="20.04.6 LTS (Focal Fossa)"
ID=ubuntu`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		_, err := GetOSVersion()
		assert.Error(t, err)
	})

	t.Run("File read error", func(t *testing.T) {
		readFile = func(name string) ([]byte, error) {
			return nil, assert.AnError
		}

		_, err := GetOSVersion()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read")
	})
}

func TestGetMachineArchitecture(t *testing.T) {
	// This test verifies the function runs - actual value depends on build arch
	arch := GetMachineArchitecture()
	assert.NotEmpty(t, arch)

	// On most common platforms it should be one of these
	validArchs := []string{"x86_64", "aarch64", "arm64", "amd64", "386", "arm"}
	assert.Contains(t, validArchs, arch)
}

func TestGetBundleIndexPath(t *testing.T) {
	// Save original readFile function and restore after test
	originalReadFile := readFile
	defer func() { readFile = originalReadFile }()

	t.Run("Ubuntu 22.04 returns correct URL", func(t *testing.T) {
		osReleaseContent := `PRETTY_NAME="Ubuntu 22.04.5 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
VERSION="22.04.5 LTS (Jammy Jellyfish)"`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		url, err := GetBundleIndexPath()
		assert.NoError(t, err)
		assert.Equal(t, DefaultBundleIndexUbuntu22, url)
	})

	t.Run("Ubuntu 24.04 returns correct URL", func(t *testing.T) {
		osReleaseContent := `PRETTY_NAME="Ubuntu 24.04 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04 LTS (Noble Numbat)"`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		url, err := GetBundleIndexPath()
		assert.NoError(t, err)
		assert.Equal(t, DefaultBundleIndexUbuntu24, url)
	})

	t.Run("Unsupported OS returns error", func(t *testing.T) {
		osReleaseContent := `PRETTY_NAME="Ubuntu 20.04 LTS"
NAME="Ubuntu"
VERSION_ID="20.04"`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		_, err := GetBundleIndexPath()
		assert.Error(t, err)
	})
}

func TestDropLastURLDir(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		wantErr  bool
	}{
		{
			name:     "standard URL with file",
			input:    "https://example.com/path/to/index.json",
			expected: "https://example.com/path/to/",
			wantErr:  false,
		},
		{
			name:     "URL with trailing slash",
			input:    "https://example.com/path/to/",
			expected: "https://example.com/path/",
			wantErr:  false,
		},
		{
			name:     "URL with single path",
			input:    "https://example.com/path",
			expected: "https://example.com/",
			wantErr:  false,
		},
		{
			name:     "URL with no path",
			input:    "https://example.com/",
			expected: "https://example.com/",
			wantErr:  false,
		},
		{
			name:     "URL with just host",
			input:    "https://example.com",
			expected: "https://example.com/",
			wantErr:  false,
		},
		{
			name:     "S3 style URL",
			input:    "https://bucket.s3.amazonaws.com/nke_bundles/Ubuntu_22.04/index.json",
			expected: "https://bucket.s3.amazonaws.com/nke_bundles/Ubuntu_22.04/",
			wantErr:  false,
		},
		{
			name:     "invalid URL",
			input:    "://invalid",
			expected: "",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := DropLastURLDir(tc.input)

			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestExtractVersion(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		expected    string
		expectError bool
	}{
		{
			name:        "standard version",
			filename:    "kubeadm_v1.28.0.tgz",
			expected:    "v1.28.0",
			expectError: false,
		},
		{
			name:        "version with git hash",
			filename:    "agent_v0.0.41-123-g1a2b3c4.tgz",
			expected:    "v0.0.41-123-g1a2b3c4",
			expectError: false,
		},
		{
			name:        "version with dirty flag",
			filename:    "bundle_v1.0.0-5-gabcdef1-dirty.tgz",
			expected:    "v1.0.0-5-gabcdef1-dirty",
			expectError: false,
		},
		{
			name:        "no version in filename",
			filename:    "bundle.tgz",
			expected:    "",
			expectError: true,
		},
		{
			name:        "malformed version",
			filename:    "bundle_1.0.tgz",
			expected:    "",
			expectError: true,
		},
		{
			name:        "full path with version",
			filename:    "path/to/kubeadm_v1.29.1.tgz",
			expected:    "v1.29.1",
			expectError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version, err := ExtractVersion(tc.filename)

			if tc.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "no version found")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, version)
			}
		})
	}
}

func TestIndexFindWithX86_64Architecture(t *testing.T) {
	index := Index{
		"kubeadm": Component{
			Entries: []Entry{
				{Arch: "x86_64", Version: semver.MustParse("v1.28.0"), Path: "bundles/kubeadm_v1.28.0_x86_64.tgz"},
			},
		},
	}

	// x86_64 should match amd64 arch labels
	bundle, found := index.Find("kubeadm", semver.MustParse("v1.28.0"), "x86_64")
	assert.True(t, found)
	assert.Equal(t, "bundles/kubeadm_v1.28.0_x86_64.tgz", bundle.Path)
}

func TestIndexFindWithAarch64Architecture(t *testing.T) {
	index := Index{
		"kubeadm": Component{
			Entries: []Entry{
				{Arch: "aarch64", Version: semver.MustParse("v1.28.0"), Path: "bundles/kubeadm_v1.28.0_aarch64.tgz"},
			},
		},
	}

	// aarch64 should match arm64 arch labels
	bundle, found := index.Find("kubeadm", semver.MustParse("v1.28.0"), "aarch64")
	assert.True(t, found)
	assert.Equal(t, "bundles/kubeadm_v1.28.0_aarch64.tgz", bundle.Path)
}

func TestIndexFindWithNAArchitecture(t *testing.T) {
	index := Index{
		"universal-bundle": Component{
			Entries: []Entry{
				{Arch: "na", Version: semver.MustParse("v1.0.0"), Path: "bundles/universal_v1.0.0.tgz"},
			},
		},
	}

	// "na" (not architecture-specific) should match both amd64 and arm64
	bundle, found := index.Find("universal-bundle", semver.MustParse("v1.0.0"), "amd64")
	assert.True(t, found)
	assert.Equal(t, "bundles/universal_v1.0.0.tgz", bundle.Path)

	bundle, found = index.Find("universal-bundle", semver.MustParse("v1.0.0"), "arm64")
	assert.True(t, found)
	assert.Equal(t, "bundles/universal_v1.0.0.tgz", bundle.Path)
}

func TestBundleStruct(t *testing.T) {
	bundle := Bundle{
		Name:           "test-bundle",
		Version:        semver.MustParse("v1.0.0"),
		InstallCmd:     []string{"./install.sh", "--arg1"},
		InstallFile:    "install.sh",
		LocalDirectory: "/tmp/test",
		Detached:       false,
		UseStaging:     true,
		Path:           "bundles/test_v1.0.0.tgz",
		URL:            "https://example.com/bundles/test_v1.0.0.tgz",
	}

	assert.Equal(t, "test-bundle", bundle.Name)
	assert.Equal(t, "1.0.0", bundle.Version.String()) // semver drops the "v" prefix
	assert.Equal(t, 2, len(bundle.InstallCmd))
	assert.True(t, bundle.UseStaging)
}

func TestProfileStruct(t *testing.T) {
	profile := Profile{
		Name:        "standard-profile",
		Version:     "v1.0.0",
		Summary:     "Standard NKE profile",
		Description: "Contains basic components for Kubernetes",
		Bundles: []Bundle{
			{Name: "kubeadm", Version: semver.MustParse("v1.28.0")},
			{Name: "agent", Version: semver.MustParse("v2.0.0")},
		},
	}

	assert.Equal(t, "standard-profile", profile.Name)
	assert.Equal(t, 2, len(profile.Bundles))
}

func TestEntryStruct(t *testing.T) {
	entry := Entry{
		Arch:    "amd64",
		Path:    "bundles/test_v1.0.0_amd64.tgz",
		Version: semver.MustParse("v1.0.0"),
	}

	assert.Equal(t, "amd64", entry.Arch)
	assert.Equal(t, "1.0.0", entry.Version.String()) // semver drops the "v" prefix
}

func TestComponentStruct(t *testing.T) {
	component := Component{
		Entries: []Entry{
			{Arch: "amd64", Version: semver.MustParse("v1.0.0"), Path: "path1"},
			{Arch: "arm64", Version: semver.MustParse("v1.0.0"), Path: "path2"},
		},
	}

	assert.Equal(t, 2, len(component.Entries))
}

func TestArchLabels(t *testing.T) {
	// Test that architecture labels are defined correctly
	assert.Contains(t, ArmArchLabels, "arm64")
	assert.Contains(t, ArmArchLabels, "aarch64")
	assert.Contains(t, ArmArchLabels, "na")

	assert.Contains(t, AmdArchLabels, "amd64")
	assert.Contains(t, AmdArchLabels, "x86_64")
	assert.Contains(t, AmdArchLabels, "na")
}

func TestConstants(t *testing.T) {
	assert.Equal(t, "nke_bundles/ubuntu_22_index.json", DefaultBundleIndexUbuntu22)
	assert.Equal(t, "nke_bundles/ubuntu_24_index.json", DefaultBundleIndexUbuntu24)
	assert.Equal(t, "https://artifacts.example.com/nke_bundles/ubuntu_22_index.json",
		JoinBundleURL("https://artifacts.example.com/", DefaultBundleIndexUbuntu22))
}

func TestParseBundleSourceURI(t *testing.T) {
	tests := []struct {
		name         string
		uri          string
		expectedType BundleSourceType
		expectedURL  string
		expectError  bool
	}{
		{
			name:         "OCI registry with oci:// prefix",
			uri:          "oci://registry.example.com/nke/bundles",
			expectedType: BundleSourceOCI,
			expectedURL:  "registry.example.com/nke/bundles",
		},
		{
			name:         "OCI registry with trailing slash",
			uri:          "oci://registry.example.com/nke/bundles/",
			expectedType: BundleSourceOCI,
			expectedURL:  "registry.example.com/nke/bundles",
		},
		{
			name:         "HTTPS URL",
			uri:          "https://artifacts.example.com/",
			expectedType: BundleSourceHTTP,
			expectedURL:  "https://artifacts.example.com/",
		},
		{
			name:         "HTTP URL",
			uri:          "http://localhost:8080/bundles/",
			expectedType: BundleSourceHTTP,
			expectedURL:  "http://localhost:8080/bundles/",
		},
		{
			name:         "File URL",
			uri:          "file:///testdata/bundles/",
			expectedType: BundleSourceFile,
			expectedURL:  "file:///testdata/bundles/",
		},
		{
			name:         "Bare domain defaults to HTTPS",
			uri:          "artifacts.example.com/",
			expectedType: BundleSourceHTTP,
			expectedURL:  "https://artifacts.example.com/",
		},
		{
			name:        "Empty URI returns error",
			uri:         "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := ParseBundleSourceURI(tt.uri)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, source)
				return
			}

			assert.NoError(t, err)
			assert.NotNil(t, source)
			assert.Equal(t, tt.expectedType, source.Type)
			assert.Equal(t, tt.expectedURL, source.URL)
			assert.Equal(t, tt.uri, source.OriginalURI)
		})
	}
}

func TestBundleSourceHelpers(t *testing.T) {
	t.Run("IsOCI returns true for OCI source", func(t *testing.T) {
		source, err := ParseBundleSourceURI("oci://nvcr.io/test")
		assert.NoError(t, err)
		assert.True(t, source.IsOCI())
		assert.False(t, source.IsHTTP())
		assert.False(t, source.IsFile())
	})

	t.Run("IsHTTP returns true for HTTP source", func(t *testing.T) {
		source, err := ParseBundleSourceURI("https://example.com/bundles/")
		assert.NoError(t, err)
		assert.False(t, source.IsOCI())
		assert.True(t, source.IsHTTP())
		assert.False(t, source.IsFile())
	})

	t.Run("IsFile returns true for file source", func(t *testing.T) {
		source, err := ParseBundleSourceURI("file:///var/bundles/")
		assert.NoError(t, err)
		assert.False(t, source.IsOCI())
		assert.False(t, source.IsHTTP())
		assert.True(t, source.IsFile())
	})
}

func TestBundleSourceTypeString(t *testing.T) {
	assert.Equal(t, "HTTP", BundleSourceHTTP.String())
	assert.Equal(t, "OCI", BundleSourceOCI.String())
	assert.Equal(t, "File", BundleSourceFile.String())
	assert.Equal(t, "Unknown", BundleSourceType(99).String())
}

// FetchBundleFromIndex had no test. These cover the two ways it resolves a
// bundle and the URL joining that used to be plain concatenation.
func TestFetchBundleFromIndex(t *testing.T) {
	index := Index{
		"calico": Component{
			Entries: []Entry{
				{Arch: "x86_64", Version: semver.MustParse("v3.31.4"), Path: "nke_bundles/x86_64/calico_v3.31.4_x86_64.tgz"},
			},
		},
	}
	body, err := json.Marshal(index)
	assert.NoError(t, err)

	const indexPath = "/nke_bundles/index.json"
	var indexHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exact path: matching any *.json would also serve the missing-index case.
		if r.URL.Path == indexPath {
			indexHits++
			_, _ = w.Write(body)
			return
		}
		// Nothing else exists, so the conventional-URL probe misses and the
		// index is what resolves the bundle.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	version := semver.MustParse("v3.31.4")
	indexURL := srv.URL + indexPath

	t.Run("resolves through the index when the conventional URL is absent", func(t *testing.T) {
		b, err := FetchBundleFromIndex(context.Background(), srv.URL+"/", indexURL, "calico", version, "x86_64")
		assert.NoError(t, err)
		assert.Equal(t, srv.URL+"/nke_bundles/x86_64/calico_v3.31.4_x86_64.tgz", b.URL)
		assert.Positive(t, indexHits, "expected the index to be fetched")
	})

	t.Run("a bundle server without a trailing slash still joins correctly", func(t *testing.T) {
		b, err := FetchBundleFromIndex(context.Background(), srv.URL, indexURL, "calico", version, "x86_64")
		assert.NoError(t, err)
		assert.Equal(t, srv.URL+"/nke_bundles/x86_64/calico_v3.31.4_x86_64.tgz", b.URL,
			"concatenation would have produced ...hostnke_bundles/...")
	})

	t.Run("a version the index does not carry is an error", func(t *testing.T) {
		_, err := FetchBundleFromIndex(context.Background(), srv.URL+"/", indexURL, "calico", semver.MustParse("v9.9.9"), "x86_64")
		assert.Error(t, err)
	})

	t.Run("an unreachable index reports the fetch failure", func(t *testing.T) {
		_, err := FetchBundleFromIndex(context.Background(), srv.URL+"/", srv.URL+"/missing/index.json", "calico", version, "x86_64")
		assert.ErrorContains(t, err, "unable to fetch index")
	})
}

func TestVersionString(t *testing.T) {
	tests := []struct {
		name    string
		version *semver.Version
		want    string
	}{
		{name: "nil version", version: nil, want: ""},
		{name: "parsed version", version: semver.MustParse("1.30.2"), want: "1.30.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, VersionString(tt.version))
		})
	}
}
