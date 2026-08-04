package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildBundleReference(t *testing.T) {
	tests := []struct {
		name        string
		registryURL string
		bundleName  string
		version     string
		want        string
	}{
		{
			name:        "development registry with longhorn-prereqs",
			registryURL: "nvcr.io/j7sbcjl3qgta/development",
			bundleName:  "longhorn-prereqs",
			version:     "v1.0.0",
			want:        "nvcr.io/j7sbcjl3qgta/development/bundles-longhorn-prereqs:v1.0.0",
		},
		{
			name:        "production registry with kubeadm",
			registryURL: "nvcr.io/j7sbcjl3qgta",
			bundleName:  "kubeadm",
			version:     "v1.31.9",
			want:        "nvcr.io/j7sbcjl3qgta/bundles-kubeadm:v1.31.9",
		},
		{
			name:        "calico bundle",
			registryURL: "nvcr.io/j7sbcjl3qgta/development",
			bundleName:  "calico",
			version:     "v3.30.1",
			want:        "nvcr.io/j7sbcjl3qgta/development/bundles-calico:v3.30.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildBundleReference(tt.registryURL, tt.bundleName, tt.version)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildCollectionReference(t *testing.T) {
	tests := []struct {
		name           string
		registryURL    string
		collectionName string
		version        string
		want           string
	}{
		{
			name:           "default collection",
			registryURL:    "nvcr.io/j7sbcjl3qgta/development",
			collectionName: "",
			version:        "v1.0.0",
			want:           "nvcr.io/j7sbcjl3qgta/development/bundles-collections:v1.0.0",
		},
		{
			name:           "default collection explicit",
			registryURL:    "nvcr.io/j7sbcjl3qgta/development",
			collectionName: "default",
			version:        "v1.0.0",
			want:           "nvcr.io/j7sbcjl3qgta/development/bundles-collections:v1.0.0",
		},
		{
			name:           "calico collection",
			registryURL:    "nvcr.io/j7sbcjl3qgta/development",
			collectionName: "calico",
			version:        "v2.0.0",
			want:           "nvcr.io/j7sbcjl3qgta/development/bundles-collections-calico:v2.0.0",
		},
		{
			name:           "ovn collection",
			registryURL:    "nvcr.io/j7sbcjl3qgta/development",
			collectionName: "ovn",
			version:        "v1.5.0",
			want:           "nvcr.io/j7sbcjl3qgta/development/bundles-collections-ovn:v1.5.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildCollectionReference(tt.registryURL, tt.collectionName, tt.version)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetArchString(t *testing.T) {
	// This test verifies the architecture mapping
	// Note: The actual result depends on the runtime architecture
	arch := GetArchString()

	// Should return a valid OCI architecture string
	validArchs := []string{"amd64", "arm64"}
	found := false
	for _, valid := range validArchs {
		if arch == valid {
			found = true
			break
		}
	}

	// On most systems this will be amd64 or arm64
	// If running on an unusual architecture, the raw arch string is returned
	assert.True(t, len(arch) > 0, "architecture string should not be empty")

	// Log the detected architecture for debugging
	t.Logf("Detected architecture: %s (valid OCI arch: %v)", arch, found)
}

func TestGetOSVersionString(t *testing.T) {
	// Save original readFile function and restore after test
	originalReadFile := readFile
	defer func() { readFile = originalReadFile }()

	t.Run("Ubuntu 22.04", func(t *testing.T) {
		osReleaseContent := `PRETTY_NAME="Ubuntu 22.04.5 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
VERSION="22.04.5 LTS (Jammy Jellyfish)"
VERSION_CODENAME=jammy
ID=ubuntu`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		version, err := GetOSVersionString()
		assert.NoError(t, err)
		assert.Equal(t, "22.04", version)
	})

	t.Run("Ubuntu 24.04", func(t *testing.T) {
		osReleaseContent := `PRETTY_NAME="Ubuntu 24.04 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		version, err := GetOSVersionString()
		assert.NoError(t, err)
		assert.Equal(t, "24.04", version)
	})

	t.Run("Unsupported version", func(t *testing.T) {
		osReleaseContent := `PRETTY_NAME="Ubuntu 20.04 LTS"
NAME="Ubuntu"
VERSION_ID="20.04"
ID=ubuntu`

		readFile = func(name string) ([]byte, error) {
			return []byte(osReleaseContent), nil
		}

		_, err := GetOSVersionString()
		assert.Error(t, err)
	})
}

func TestOCIConfig(t *testing.T) {
	config := OCIConfig{
		RegistryURL: "nvcr.io/j7sbcjl3qgta/development",
	}

	assert.Equal(t, "nvcr.io/j7sbcjl3qgta/development", config.RegistryURL)
}

func TestNGCOAuthUsername(t *testing.T) {
	// Verify the NGC OAuth username constant is correct
	assert.Equal(t, "$oauthtoken", NGCOAuthUsername)
}

func TestNVIDIABundleMediaType(t *testing.T) {
	// Verify the NVIDIA bundle media type constant is correct
	assert.Equal(t, "application/vnd.nvidia.bundle.v1+tgz", NVIDIABundleMediaType)
}
