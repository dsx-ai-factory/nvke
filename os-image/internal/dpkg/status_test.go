// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dpkg

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		want          []Package
		wantHeld      []bool
		wantInstalled []bool
		wantErr       error
		wantErrText   string
	}{
		{
			name: "installed",
			input: `Package: bash
Status: install ok installed
Priority: required
Architecture: amd64
Version: 5.2.21-2ubuntu4
`,
			want:          []Package{{Name: "bash", Version: "5.2.21-2ubuntu4", Architecture: "amd64", Status: Status{WantInstall, FlagOK, StateInstalled}}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{true},
		},
		{
			name: "held and installed",
			input: `Package: kubelet
Status: hold ok installed
Architecture: amd64
Version: 1.33.1-1.1
`,
			want:          []Package{{Name: "kubelet", Version: "1.33.1-1.1", Architecture: "amd64", Status: Status{WantHold, FlagOK, StateInstalled}}},
			wantHeld:      []bool{true},
			wantInstalled: []bool{true},
		},
		{
			name: "unpacked",
			input: `Package: nvke-agent
Status: install ok unpacked
Architecture: arm64
Version: 0.9.0
`,
			want:          []Package{{Name: "nvke-agent", Version: "0.9.0", Architecture: "arm64", Status: Status{WantInstall, FlagOK, StateUnpacked}}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{false},
		},
		{
			name: "half-configured",
			input: `Package: nvidia-dkms-570
Status: install ok half-configured
Architecture: amd64
Source: nvidia-graphics-drivers-570
Version: 570.133.20-0ubuntu1
`,
			want: []Package{{
				Name: "nvidia-dkms-570", Version: "570.133.20-0ubuntu1", Architecture: "amd64",
				Source: "nvidia-graphics-drivers-570", SourceVersion: "570.133.20-0ubuntu1",
				Status: Status{WantInstall, FlagOK, StateHalfConfigured},
			}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{false},
		},
		{
			name: "half-installed needing reinstall",
			input: `Package: containerd
Status: install reinstreq half-installed
Architecture: amd64
Version: 1.7.27-0ubuntu1
`,
			want:          []Package{{Name: "containerd", Version: "1.7.27-0ubuntu1", Architecture: "amd64", Status: Status{WantInstall, FlagReinstReq, StateHalfInstalled}}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{false},
		},
		{
			name:          "half-installed without version or architecture",
			input:         "Package: containerd\nStatus: install reinstreq half-installed\n",
			want:          []Package{{Name: "containerd", Status: Status{WantInstall, FlagReinstReq, StateHalfInstalled}}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{false},
		},
		{
			name:          "field names are case-insensitive",
			input:         "package: bash\nSTATUS: hold ok installed\nArchitecture: amd64\nversion: 5.2\n",
			want:          []Package{{Name: "bash", Version: "5.2", Architecture: "amd64", Status: Status{WantHold, FlagOK, StateInstalled}}},
			wantHeld:      []bool{true},
			wantInstalled: []bool{true},
		},
		{
			name: "source version after a tab or several spaces",
			input: "Package: zlib1g\nStatus: install ok installed\nArchitecture: amd64\nVersion: 1.3+b1\nSource: zlib\t(1.3)\n\n" +
				"Package: zlib1g-dev\nStatus: install ok installed\nArchitecture: amd64\nVersion: 1.3+b1\nSource: zlib   (1.3)\n",
			want: []Package{
				{Name: "zlib1g", Version: "1.3+b1", Architecture: "amd64", Source: "zlib", SourceVersion: "1.3", Status: Status{WantInstall, FlagOK, StateInstalled}},
				{Name: "zlib1g-dev", Version: "1.3+b1", Architecture: "amd64", Source: "zlib", SourceVersion: "1.3", Status: Status{WantInstall, FlagOK, StateInstalled}},
			},
			wantHeld:      []bool{false, false},
			wantInstalled: []bool{true, true},
		},
		{
			name: "installed but reinstall required",
			input: `Package: containerd
Status: install reinstreq installed
Architecture: amd64
Version: 1.7.27-0ubuntu1
`,
			want:          []Package{{Name: "containerd", Version: "1.7.27-0ubuntu1", Architecture: "amd64", Status: Status{WantInstall, FlagReinstReq, StateInstalled}}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{false},
		},
		{
			name: "config-files",
			input: `Package: snapd
Status: deinstall ok config-files
Architecture: amd64
Version: 2.66.1+24.04
Config-Version: 2.66.1+24.04
`,
			want:          []Package{{Name: "snapd", Version: "2.66.1+24.04", Architecture: "amd64", Status: Status{WantDeinstall, FlagOK, StateConfigFiles}}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{false},
		},
		{
			name: "not-installed hold is reported",
			input: `Package: linux-image-generic
Status: hold ok not-installed
`,
			want:          []Package{{Name: "linux-image-generic", Status: Status{WantHold, FlagOK, StateNotInstalled}}},
			wantHeld:      []bool{true},
			wantInstalled: []bool{false},
		},
		{
			name: "multi-arch same name sorted by arch, packages sorted by name",
			input: `Package: zlib1g
Status: install ok installed
Architecture: i386
Multi-Arch: same
Source: zlib (1:1.3.dfsg-3.1ubuntu2)
Version: 1:1.3.dfsg-3.1ubuntu2.1

Package: zlib1g
Status: install ok installed
Architecture: amd64
Multi-Arch: same
Source: zlib (1:1.3.dfsg-3.1ubuntu2)
Version: 1:1.3.dfsg-3.1ubuntu2.1

Package: apt
Status: install ok installed
Architecture: amd64
Version: 2.8.3
`,
			want: []Package{
				{Name: "apt", Version: "2.8.3", Architecture: "amd64", Status: Status{WantInstall, FlagOK, StateInstalled}},
				{
					Name: "zlib1g", Version: "1:1.3.dfsg-3.1ubuntu2.1", Architecture: "amd64",
					Source: "zlib", SourceVersion: "1:1.3.dfsg-3.1ubuntu2",
					Status: Status{WantInstall, FlagOK, StateInstalled},
				},
				{
					Name: "zlib1g", Version: "1:1.3.dfsg-3.1ubuntu2.1", Architecture: "i386",
					Source: "zlib", SourceVersion: "1:1.3.dfsg-3.1ubuntu2",
					Status: Status{WantInstall, FlagOK, StateInstalled},
				},
			},
			wantHeld:      []bool{false, false, false},
			wantInstalled: []bool{true, true, true},
		},
		{
			name: "continuation lines and conffiles are skipped",
			input: `Package: openssh-server
Status: install ok installed
Architecture: amd64
Version: 1:9.6p1-3ubuntu13.5
Conffiles:
 /etc/default/ssh 500e3a4a7a6e4d9d4b6b6a8c1d6b4b7f
 /etc/init.d/ssh 3649a6fe8c18ad1d5245fd91737de507
Description: secure shell (SSH) server
 This is the portable version of OpenSSH.
 .
 Version: not-a-field
Homepage: https://www.openssh.com/
`,
			want:          []Package{{Name: "openssh-server", Version: "1:9.6p1-3ubuntu13.5", Architecture: "amd64", Status: Status{WantInstall, FlagOK, StateInstalled}}},
			wantHeld:      []bool{false},
			wantInstalled: []bool{true},
		},
		{
			name:          "extra blank lines and CRLF",
			input:         "\n\nPackage: a\r\nStatus: install ok installed\r\nArchitecture: all\r\nVersion: 1\r\n\n\n\nPackage: b\nStatus: install ok installed\nArchitecture: all\nVersion: 2",
			want:          []Package{{Name: "a", Version: "1", Architecture: "all", Status: Status{WantInstall, FlagOK, StateInstalled}}, {Name: "b", Version: "2", Architecture: "all", Status: Status{WantInstall, FlagOK, StateInstalled}}},
			wantHeld:      []bool{false, false},
			wantInstalled: []bool{true, true},
		},
		{name: "empty file", input: "", wantErr: ErrEmpty},
		{name: "only blank lines", input: "\n \n\n", wantErr: ErrEmpty},
		{name: "line without colon", input: "Package: a\nStatus install ok installed\n", wantErr: ErrMalformed, wantErrText: "line 2"},
		{name: "continuation with no field", input: " orphan\nPackage: a\n", wantErr: ErrMalformed, wantErrText: "continuation"},
		{name: "missing Package", input: "Status: install ok installed\nVersion: 1\n", wantErr: ErrMalformed, wantErrText: "no Package field"},
		{name: "missing Status", input: "Package: a\nArchitecture: all\nVersion: 1\n", wantErr: ErrMalformed, wantErrText: "no Status field"},
		{name: "status not three words", input: "Package: a\nStatus: install installed\nArchitecture: all\nVersion: 1\n", wantErr: ErrMalformed, wantErrText: "not three words"},
		{name: "unknown want", input: "Package: a\nStatus: keep ok installed\nArchitecture: all\nVersion: 1\n", wantErr: ErrMalformed, wantErrText: "unknown want"},
		{name: "unknown flag", input: "Package: a\nStatus: install bad installed\nArchitecture: all\nVersion: 1\n", wantErr: ErrMalformed, wantErrText: "unknown flag"},
		{name: "unknown state", input: "Package: a\nStatus: install ok broken\nArchitecture: all\nVersion: 1\n", wantErr: ErrMalformed, wantErrText: "unknown state"},
		{name: "installed without version", input: "Package: a\nStatus: install ok installed\nArchitecture: all\n", wantErr: ErrMalformed, wantErrText: "lacks Version"},
		{
			name:        "field repeated in one stanza",
			input:       "Package: alpha\nStatus: install ok installed\nArchitecture: all\nVersion: 1\npackage: beta\n",
			wantErr:     ErrMalformed,
			wantErrText: "line 5",
		},
		{
			name:        "duplicate name and arch",
			input:       "Package: a\nStatus: install ok installed\nArchitecture: all\nVersion: 1\n\nPackage: a\nStatus: install ok installed\nArchitecture: all\nVersion: 2\n",
			wantErr:     ErrMalformed,
			wantErrText: "appears twice",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tc.input))
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.wantErrText)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Packages)
			for i, p := range got.Packages {
				assert.Equal(t, tc.wantHeld[i], p.Held(), "Held() of %s:%s", p.Name, p.Architecture)
				assert.Equal(t, tc.wantInstalled[i], p.Installed(), "Installed() of %s:%s", p.Name, p.Architecture)
			}
		})
	}
}

const validStatus = "Package: bash\nStatus: hold ok installed\nArchitecture: amd64\nVersion: 5.2\n"

func TestReadFS(t *testing.T) {
	tests := []struct {
		name     string
		fsys     fstest.MapFS
		wantName string
		wantErr  error
	}{
		{name: "reads status", fsys: fstest.MapFS{StatusPath: {Data: []byte(validStatus)}}, wantName: "bash"},
		{name: "missing file", fsys: fstest.MapFS{}, wantErr: fs.ErrNotExist},
		{name: "empty file", fsys: fstest.MapFS{StatusPath: {Data: nil}}, wantErr: ErrEmpty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadFS(tc.fsys)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, got.Packages, 1)
			assert.Equal(t, tc.wantName, got.Packages[0].Name)
		})
	}
}

func TestReadRoot(t *testing.T) {
	tests := []struct {
		name    string
		status  *string
		wantErr error
	}{
		{name: "reads root", status: new(validStatus)},
		{name: "missing status names the path", wantErr: fs.ErrNotExist},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.status != nil {
				path := filepath.Join(root, StatusPath)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(*tc.status), 0o600))
			}
			got, err := ReadRoot(root)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), filepath.Join(root, StatusPath))
				return
			}
			require.NoError(t, err)
			require.Len(t, got.Packages, 1)
			assert.True(t, got.Packages[0].Held())
		})
	}
}

func TestManifestJSON(t *testing.T) {
	m := Manifest{Packages: []Package{
		{Name: "bash", Version: "5.2", Architecture: "amd64", Status: Status{WantHold, FlagOK, StateInstalled}},
		{Name: "gone", Status: Status{WantPurge, FlagOK, StateNotInstalled}},
	}}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	assert.JSONEq(t, `{"packages":[
		{"name":"bash","version":"5.2","architecture":"amd64","status":{"want":"hold","flag":"ok","state":"installed"}},
		{"name":"gone","status":{"want":"purge","flag":"ok","state":"not-installed"}}
	]}`, string(raw))
	assert.Equal(t, "hold ok installed", m.Packages[0].Status.String())
}
