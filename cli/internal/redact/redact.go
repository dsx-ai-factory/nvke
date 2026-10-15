// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package redact masks credentials in text and files that nvkectl prints or saves.
//
// Credentials are registered as the process generates them, so every caller
// masks the same values even when they are in different engines.
package redact

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Placeholder replaces a credential wherever it would otherwise be printed or saved.
const Placeholder = "<redacted>"

// secrets holds the credentials this process generated. It is process-wide
// because the engine that installs the releases is not the one that later
// collects diagnostics, and both must mask the same values. The process is a
// short-lived CLI, so the set is never pruned.
var secrets = struct {
	sync.Mutex
	values []string
}{}

// RegisterSecret records a credential so that Text and Tree mask it.
// An empty value is ignored: masking "" would mangle every string.
func RegisterSecret(value string) {
	if value == "" {
		return
	}
	secrets.Lock()
	defer secrets.Unlock()
	for _, v := range secrets.values {
		if v == value {
			return
		}
	}
	secrets.values = append(secrets.values, value)
}

// connectionURLPassword matches the password in scheme://user:password@host.
// Pod specs carry the database URL this way.
var connectionURLPassword = regexp.MustCompile(`(postgres(?:ql)?://[^:/\s@"]+:)[^@/\s"]+(@)`)

// Text masks every registered credential and any password in a connection URL
// in text.
func Text(text string) string {
	secrets.Lock()
	values := append([]string(nil), secrets.values...)
	secrets.Unlock()
	for _, v := range values {
		text = strings.ReplaceAll(text, v, Placeholder)
	}
	return connectionURLPassword.ReplaceAllString(text, "${1}"+Placeholder+"${2}")
}

// Tree rewrites, in place, every file under dir that holds a credential.
// It is best-effort and returns the paths it could not scrub: files it could
// not rewrite and directories it could not read.
func Tree(dir string) (failed []string) {
	// WalkDir does not follow a symlink, the root included, so a symlinked dir
	// would be skipped with nothing scrubbed and nothing reported.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return []string{dir}
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return []string{dir}
	}
	// Paths are reported under dir as the caller gave it, not under the
	// resolved root, which can be a different spelling of the same place.
	named := func(path string) string {
		if rel, err := filepath.Rel(root, path); err == nil {
			return filepath.Join(dir, rel)
		}
		return path
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory hides every file under it, so it is as
			// much a miss as a file that cannot be rewritten.
			failed = append(failed, named(path))
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			failed = append(failed, named(path))
			return nil
		}
		if clean := Text(string(body)); clean != string(body) {
			info, statErr := d.Info()
			if statErr != nil || os.WriteFile(path, []byte(clean), info.Mode().Perm()) != nil {
				failed = append(failed, named(path))
			}
		}
		return nil
	})
	return failed
}
