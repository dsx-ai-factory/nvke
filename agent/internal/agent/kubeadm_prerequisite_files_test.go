// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

func TestWriteKubeadmFilesValidations(t *testing.T) {
	rejectionCases := []struct {
		name    string
		files   []*pb.File
		wantErr string
	}{
		{
			name:    "non absolute path",
			files:   []*pb.File{{Path: "etc/kubernetes/config.yaml"}},
			wantErr: "path must be absolute",
		},
		{
			name: "empty path",
			files: []*pb.File{{
				Path:    "",
				Content: []byte("ok"),
				Mode:    0o644,
				Owner:   "root:root",
			}},
			wantErr: "path is required",
		},
		{
			name:    "nil file entry",
			files:   []*pb.File{nil},
			wantErr: "file entry 0 is nil",
		},
		{
			name: "nil file entry after valid entry reports index",
			files: []*pb.File{
				{
					Path:    "/etc/kubernetes/first.yaml",
					Content: []byte("ok"),
					Mode:    0o644,
					Owner:   "root:root",
				},
				nil,
			},
			wantErr: "file entry 1 is nil",
		},
		{
			name:    "directory target",
			files:   []*pb.File{{Path: "/"}},
			wantErr: "destination is a directory",
		},
		{
			name: "duplicate cleaned paths",
			files: []*pb.File{
				{Path: "/etc/kubernetes/prereq.yaml", Mode: 0o644, Owner: "root:root"},
				{Path: "/etc/kubernetes/../kubernetes/prereq.yaml", Mode: 0o644, Owner: "root:root"},
			},
			wantErr: "duplicate path",
		},
		{
			name: "single file content over 1 MiB",
			files: []*pb.File{
				{Path: "/etc/kubernetes/a", Content: make([]byte, maxKubeadmFileBytes+1)},
			},
			wantErr: "file /etc/kubernetes/a content exceeds max size (1048576 bytes): got 1048577 bytes",
		},
		{
			name: "missing mode",
			files: []*pb.File{{
				Path:    "/etc/kubernetes/default-mode.yaml",
				Content: []byte("ok"),
				Owner:   "root:root",
			}},
			wantErr: "mode is required",
		},
		{
			name: "mode with bits outside 0777",
			files: []*pb.File{{
				Path: "/etc/kubernetes/invalid-mode.yaml",
				Mode: 0o1000,
			}},
			wantErr: "mode must contain only permission bits",
		},
		{
			name: "missing owner",
			files: []*pb.File{{
				Path:    "/etc/kubernetes/default-owner.yaml",
				Content: []byte("ok"),
				Mode:    0o644,
			}},
			wantErr: "owner is required",
		},
		{
			name: "invalid owner format",
			files: []*pb.File{{
				Path:  "/etc/kubernetes/owner.yaml",
				Mode:  0o644,
				Owner: "root",
			}},
			wantErr: "owner must use user:group format",
		},
		{
			name: "owner with empty user name",
			files: []*pb.File{{
				Path:  "/etc/kubernetes/owner.yaml",
				Mode:  0o644,
				Owner: ":staff",
			}},
			wantErr: "owner must use user:group format",
		},
		{
			name: "owner with empty group name",
			files: []*pb.File{{
				Path:  "/etc/kubernetes/owner.yaml",
				Mode:  0o644,
				Owner: "alice:",
			}},
			wantErr: "owner must use user:group format",
		},
		{
			name: "unknown user",
			files: []*pb.File{{
				Path:  "/etc/kubernetes/owner.yaml",
				Mode:  0o644,
				Owner: "missing:staff",
			}},
			wantErr: `looking up user "missing"`,
		},
		{
			name: "unknown group",
			files: []*pb.File{{
				Path:  "/etc/kubernetes/owner.yaml",
				Mode:  0o644,
				Owner: "alice:missing",
			}},
			wantErr: `looking up group "missing"`,
		},
	}
	for _, tc := range rejectionCases {
		t.Run(tc.name, func(t *testing.T) {
			server := newKubeadmFileTestServer(t)
			err := server.writeFilesToDisk(tc.files)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestWriteKubeadmFiles(t *testing.T) {
	t.Run("allowed absolute path is written successfully", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/oidc/auth-config.yaml",
			Content: []byte("issuer: https://issuer.example.com\n"),
			Mode:    0o600,
			Owner:   "alice:staff",
		}})
		require.NoError(t, err)

		path := filepath.Join(server.RootPath, "etc", "kubernetes", "oidc", "auth-config.yaml")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "issuer: https://issuer.example.com\n", string(data))
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("missing parent directories are created", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/a/b/c/config.yaml",
			Content: []byte("ok"),
			Mode:    0o644,
			Owner:   "root:root",
		}})
		require.NoError(t, err)

		info, err := os.Stat(filepath.Join(server.RootPath, "etc", "kubernetes", "a", "b", "c"))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	})

	t.Run("root path is honored", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/var/lib/nke/custom/prereq.json",
			Content: []byte("{}"),
			Mode:    0o644,
			Owner:   "root:root",
		}})
		require.NoError(t, err)

		_, err = os.Stat(filepath.Join(server.RootPath, "var", "lib", "nke", "custom", "prereq.json"))
		assert.NoError(t, err)
	})

	t.Run("existing symlink in parent chain is rejected", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		targetDir := filepath.Join(server.RootPath, "real-etc")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))
		require.NoError(t, os.Symlink(targetDir, filepath.Join(server.RootPath, "etc")))

		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/prereq.yaml",
			Content: []byte("ok"),
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is a symlink")
	})

	t.Run("parent path segment that is a file is rejected", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		require.NoError(t, os.MkdirAll(filepath.Join(server.RootPath, "etc"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(server.RootPath, "etc", "kubernetes"), []byte("not-a-directory"), 0o644))

		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/prereq.yaml",
			Content: []byte("ok"),
			Mode:    0o644,
			Owner:   "root:root",
		}})
		require.Error(t, err)
		want := fmt.Sprintf(
			"invalid destination %s: parent path %s is not a directory",
			"/etc/kubernetes/prereq.yaml",
			filepath.Join(server.RootPath, "etc", "kubernetes"),
		)
		assert.Equal(t, want, err.Error())
	})

	t.Run("existing symlink destination is rejected", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		destDir := filepath.Join(server.RootPath, "etc", "kubernetes")
		require.NoError(t, os.MkdirAll(destDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(server.RootPath, "target"), []byte("ok"), 0o644))
		require.NoError(t, os.Symlink(filepath.Join(server.RootPath, "target"), filepath.Join(destDir, "prereq.yaml")))

		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/prereq.yaml",
			Content: []byte("ok"),
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "destination is a symlink")
	})

	t.Run("existing directory destination is rejected", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		require.NoError(t, os.MkdirAll(filepath.Join(server.RootPath, "etc", "kubernetes", "prereq.yaml"), 0o755))

		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/prereq.yaml",
			Content: []byte("ok"),
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "destination is a directory")
	})

	t.Run("existing non-regular file destination is rejected", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		destDir := filepath.Join(server.RootPath, "etc", "kubernetes")
		require.NoError(t, os.MkdirAll(destDir, 0o755))
		require.NoError(t, syscall.Mkfifo(filepath.Join(destDir, "prereq.yaml"), 0o644))

		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/prereq.yaml",
			Content: []byte("ok"),
			Mode:    0o644,
			Owner:   "root:root",
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "destination exists and is not a regular file")
	})

	t.Run("chmod and chown happen on the temp file before rename", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		finalPath := filepath.Join(server.RootPath, "etc", "kubernetes", "prereq.yaml")
		var gotUID, gotGID int
		server.kubeadmFileOps.chown = func(path string, uid, gid int) error {
			gotUID = uid
			gotGID = gid
			assert.NotEqual(t, finalPath, path)
			_, err := os.Stat(finalPath)
			assert.ErrorIs(t, err, os.ErrNotExist)

			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
			return nil
		}

		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/prereq.yaml",
			Content: []byte("ok"),
			Mode:    0o640,
			Owner:   "alice:staff",
		}})
		require.NoError(t, err)
		assert.Equal(t, 1001, gotUID)
		assert.Equal(t, 1002, gotGID)

		info, err := os.Stat(finalPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	})

	t.Run("write replaces existing regular file", func(t *testing.T) {
		server := newKubeadmFileTestServer(t)
		finalPath := filepath.Join(server.RootPath, "etc", "kubernetes", "prereq.yaml")
		require.NoError(t, os.MkdirAll(filepath.Dir(finalPath), 0o755))
		require.NoError(t, os.WriteFile(finalPath, []byte("before"), 0o644))

		err := server.writeFilesToDisk([]*pb.File{{
			Path:    "/etc/kubernetes/prereq.yaml",
			Content: []byte("after"),
			Mode:    0o644,
			Owner:   "root:root",
		}})
		require.NoError(t, err)
		got, err := os.ReadFile(finalPath)
		require.NoError(t, err)
		assert.Equal(t, "after", string(got))
	})
}

// resolvedTempDir is t.TempDir() with symlinks resolved. writeKubeadmFiles walks
// every parent of a destination and refuses a symlink, which is what stops a
// planted link redirecting a write. On macOS t.TempDir() sits under /var, which
// is itself a symlink to /private/var, so the check fires on the harness rather
// than on anything the test set up.
func resolvedTempDir(t *testing.T) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return resolved
}

func newKubeadmFileTestServer(t *testing.T) *NodeAgentServer {
	t.Helper()

	return &NodeAgentServer{
		RootPath:   resolvedTempDir(t),
		Persister:  &MockPersister{},
		FileWriter: &MockFileWriter{},
		kubeadmFileOps: fileOps{
			lookupUser: func(name string) (*user.User, error) {
				switch name {
				case "root":
					return &user.User{Username: "root", Uid: "0"}, nil
				case "alice":
					return &user.User{Username: "alice", Uid: "1001"}, nil
				default:
					return nil, user.UnknownUserError(name)
				}
			},
			lookupGroup: func(name string) (*user.Group, error) {
				switch name {
				case "root":
					return &user.Group{Name: "root", Gid: "0"}, nil
				case "staff":
					return &user.Group{Name: "staff", Gid: "1002"}, nil
				default:
					return nil, user.UnknownGroupError(name)
				}
			},
			chown: func(string, int, int) error {
				return nil
			},
		},
	}
}
