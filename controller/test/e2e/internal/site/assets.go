// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// archiveAssets tars rels, relative to root, keeping their paths. It skips
// what a macOS checkout carries that the node must not see: AppleDouble
// files, which helm reads as broken templates, and .git.
func archiveAssets(root string, rels []string) ([]byte, error) {
	if len(rels) == 0 {
		return nil, fmt.Errorf("no asset paths under %s", root)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, rel := range rels {
		start := filepath.Join(root, filepath.Clean(rel))
		if err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if skipAsset(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return addAsset(tw, root, p, d)
		}); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func skipAsset(name string) bool {
	return strings.HasPrefix(name, "._") || name == ".DS_Store" || name == ".git"
}

func addAsset(tw *tar.Writer, root, p string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	var link string
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(p); err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return err
	}
	hdr.Name = filepath.ToSlash(rel)
	if d.IsDir() {
		hdr.Name += "/"
	}
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	if werr := tw.WriteHeader(hdr); werr != nil {
		return werr
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	return err
}
