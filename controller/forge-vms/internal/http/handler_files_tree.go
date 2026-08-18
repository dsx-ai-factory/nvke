// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"fmt"
	nethttp "net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type FilesTreeNode struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Type     string          `json:"type"`
	Size     int64           `json:"size,omitempty"`
	ModTime  time.Time       `json:"modTime,omitempty"`
	Children []FilesTreeNode `json:"children,omitempty"`
}

type FilesTreeResponse struct {
	RootPath string        `json:"rootPath"`
	Exists   bool          `json:"exists"`
	Root     FilesTreeNode `json:"root"`
}

func (s *Server) handleFilesTree(w nethttp.ResponseWriter, _ *nethttp.Request) {
	rootName := filepath.Base(filepath.Clean(s.filesDir))
	if rootName == "." || rootName == string(filepath.Separator) {
		rootName = s.filesDir
	}

	resp := FilesTreeResponse{
		RootPath: s.filesDir,
		Root: FilesTreeNode{
			Name: rootName,
			Path: "",
			Type: "dir",
		},
	}

	if s.filesDir == "" {
		writeJSON(w, nethttp.StatusOK, resp)
		return
	}

	info, err := os.Stat(s.filesDir)
	switch {
	case os.IsNotExist(err):
		writeJSON(w, nethttp.StatusOK, resp)
		return
	case err != nil:
		nethttp.Error(w, fmt.Sprintf("failed to stat files root: %v", err), nethttp.StatusInternalServerError)
		return
	case !info.IsDir():
		nethttp.Error(w, "configured files root is not a directory", nethttp.StatusInternalServerError)
		return
	}

	rootNode, err := buildFilesTreeNode(s.filesDir, s.filesDir)
	if err != nil {
		nethttp.Error(w, fmt.Sprintf("failed to build files tree: %v", err), nethttp.StatusInternalServerError)
		return
	}

	resp.Exists = true
	resp.Root = rootNode
	writeJSON(w, nethttp.StatusOK, resp)
}

func buildFilesTreeNode(rootDir, absPath string) (FilesTreeNode, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return FilesTreeNode{}, err
	}

	relPath, err := filepath.Rel(rootDir, absPath)
	if err != nil {
		return FilesTreeNode{}, err
	}
	if relPath == "." {
		relPath = ""
	}

	node := FilesTreeNode{
		Name:    filepath.Base(absPath),
		Path:    filepath.ToSlash(relPath),
		ModTime: info.ModTime(),
	}

	if info.IsDir() {
		node.Type = "dir"

		entries, err := os.ReadDir(absPath)
		if err != nil {
			return FilesTreeNode{}, err
		}

		children := make([]FilesTreeNode, 0, len(entries))
		for _, entry := range entries {
			childPath := filepath.Join(absPath, entry.Name())
			childNode, err := buildFilesTreeNode(rootDir, childPath)
			if err != nil {
				return FilesTreeNode{}, err
			}
			children = append(children, childNode)
		}

		sort.Slice(children, func(i, j int) bool {
			if children[i].Type != children[j].Type {
				return children[i].Type == "dir"
			}
			return children[i].Name < children[j].Name
		})

		node.Children = children
		return node, nil
	}

	node.Type = "file"
	node.Size = info.Size()
	return node, nil
}
