// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	maxKubeadmFileBytes = 1 << 20 // 1 MiB
)

type fileOps struct {
	lookupUser  func(string) (*user.User, error)
	lookupGroup func(string) (*user.Group, error)
	chown       func(string, int, int) error
}

func newDefaultFileOps() fileOps {
	return fileOps{
		lookupUser:  user.Lookup,
		lookupGroup: user.LookupGroup,
		chown:       os.Chown,
	}
}

type fileSpec struct {
	path    string
	content []byte
	mode    os.FileMode
	uid     int
	gid     int
}

func (s *NodeAgentServer) writeFilesToDisk(files []*pb.File) error {
	validated, err := s.validateFiles(files)
	if err != nil {
		return err
	}

	for _, file := range validated {
		if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
			return fmt.Errorf("creating parent directory for %s: %w", file.path, err)
		}
		if err := atomicWriteFileWithOwnership(file, s.kubeadmFileOps); err != nil {
			return fmt.Errorf("writing file %s: %w", file.path, err)
		}
	}

	return nil
}

func (s *NodeAgentServer) validateFiles(files []*pb.File) ([]fileSpec, error) {
	if len(files) == 0 {
		return nil, nil
	}

	seenPaths := make(map[string]struct{}, len(files))
	validated := make([]fileSpec, 0, len(files))

	for i, f := range files {
		if f == nil {
			return nil, fmt.Errorf("file entry %d is nil", i)
		}

		if len(f.GetContent()) > maxKubeadmFileBytes {
			return nil, fmt.Errorf(
				"file %s content exceeds max size (%d bytes): got %d bytes",
				f.GetPath(),
				maxKubeadmFileBytes,
				len(f.GetContent()),
			)
		}

		cleanedPath, err := validateKubeadmFilePath(f.GetPath(), seenPaths)
		if err != nil {
			return nil, err
		}

		resolvedPath := s.resolveFilePath(cleanedPath)
		if err := validateKubeadmFileDestination(resolvedPath); err != nil {
			return nil, fmt.Errorf("invalid destination %s: %w", cleanedPath, err)
		}

		mode, err := validateKubeadmFileMode(f.GetMode())
		if err != nil {
			return nil, fmt.Errorf("invalid mode for %s: %w", cleanedPath, err)
		}

		uid, gid, err := s.resolveKubeadmFileOwner(f.GetOwner())
		if err != nil {
			return nil, fmt.Errorf("invalid owner for %s: %w", cleanedPath, err)
		}

		seenPaths[cleanedPath] = struct{}{}
		validated = append(validated, fileSpec{
			path:    resolvedPath,
			content: f.GetContent(),
			mode:    mode,
			uid:     uid,
			gid:     gid,
		})
	}

	return validated, nil
}

func validateKubeadmFilePath(path string, seenPaths map[string]struct{}) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}

	cleanedPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanedPath) {
		return "", fmt.Errorf("path must be absolute: %q", path)
	}
	if _, exists := seenPaths[cleanedPath]; exists {
		return "", fmt.Errorf("duplicate path %s", cleanedPath)
	}

	return cleanedPath, nil
}

func validateKubeadmFileMode(mode uint32) (os.FileMode, error) {
	if mode == 0 {
		return 0, fmt.Errorf("mode is required")
	}
	if mode&^0o777 != 0 {
		return 0, fmt.Errorf("mode must contain only permission bits")
	}
	return os.FileMode(mode), nil
}

func (s *NodeAgentServer) resolveKubeadmFileOwner(owner string) (int, int, error) {
	if owner == "" {
		return 0, 0, fmt.Errorf("owner is required")
	}
	if strings.Count(owner, ":") != 1 {
		return 0, 0, fmt.Errorf("owner must use user:group format")
	}

	userName, groupName, _ := strings.Cut(owner, ":")
	if userName == "" || groupName == "" {
		return 0, 0, fmt.Errorf("owner must use user:group format")
	}

	usr, err := s.kubeadmFileOps.lookupUser(userName)
	if err != nil {
		return 0, 0, fmt.Errorf("looking up user %q: %w", userName, err)
	}
	grp, err := s.kubeadmFileOps.lookupGroup(groupName)
	if err != nil {
		return 0, 0, fmt.Errorf("looking up group %q: %w", groupName, err)
	}

	uid, err := strconv.Atoi(usr.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing uid %q: %w", usr.Uid, err)
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing gid %q: %w", grp.Gid, err)
	}

	return uid, gid, nil
}

func (s *NodeAgentServer) resolveFilePath(path string) string {
	if s.RootPath == "" {
		return path
	}
	return filepath.Join(s.RootPath, path)
}

func validateKubeadmFileDestination(path string) error {
	if err := validateNoSymlinkParents(path); err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("destination is a symlink")
		}
		if info.IsDir() {
			return fmt.Errorf("destination is a directory")
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("destination exists and is not a regular file")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("statting destination: %w", err)
	}

	return nil
}

func validateNoSymlinkParents(path string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("parent path %s is a symlink", current)
			}
			if !info.IsDir() {
				return fmt.Errorf("parent path %s is not a directory", current)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat parent path %s: %w", current, err)
		}

		next := filepath.Dir(current)
		if next == current {
			return nil
		}
	}
}

func atomicWriteFileWithOwnership(
	file fileSpec,
	ops fileOps,
) error {
	dir := filepath.Dir(file.path)
	tmp, err := os.CreateTemp(dir, ".kubeadm-file-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = tmp.Close() }()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(file.content); err != nil {
		return fmt.Errorf("writing content to temp file %s for %s: %w", tmpName, file.path, err)
	}
	if err := tmp.Chmod(file.mode); err != nil {
		return fmt.Errorf("setting mode %o on temp file %s for %s: %w", file.mode, tmpName, file.path, err)
	}
	if err := ops.chown(tmpName, file.uid, file.gid); err != nil {
		return fmt.Errorf(
			"setting ownership uid=%d gid=%d on temp file %s for %s: %w",
			file.uid,
			file.gid,
			tmpName,
			file.path,
			err,
		)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing temp file %s for %s to disk: %w", tmpName, file.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing intermediate temp file for %s: %w", file.path, err)
	}

	return os.Rename(tmpName, file.path)
}
