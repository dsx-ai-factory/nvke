// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package redact masks credentials in text, files and log records that nvkectl
// prints, saves or exports.
//
// Credentials are registered as the process generates them, so every caller
// masks the same values even when they are in different engines.
package redact

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// values is sorted longest first, so that a secret inside a longer one cannot
	// split it and leave part of the longer one in the text. RegisterSecret
	// replaces it instead of changing it, so Text can read it without copying.
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
	if slices.Contains(secrets.values, value) {
		return
	}
	at, _ := slices.BinarySearchFunc(secrets.values, value, func(have, want string) int { return len(want) - len(have) })
	secrets.values = slices.Insert(slices.Clone(secrets.values), at, value)
}

// connectionURLPassword matches the password in scheme://user:password@host.
// Pod specs carry the database URL this way.
var connectionURLPassword = regexp.MustCompile(`(postgres(?:ql)?://[^:/\s@"]+:)[^@/\s"]+(@)`)

// Text masks every registered credential and any password in a connection URL
// in text.
func Text(text string) string {
	secrets.Lock()
	values := secrets.values
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

// Record returns a copy of record with the registered credentials masked in its
// message and attributes, for a handler that does not use ReplaceAttr, such as
// the OpenTelemetry exporter's. record itself is unchanged.
func Record(record slog.Record) slog.Record {
	masked := slog.NewRecord(record.Time, record.Level, Text(record.Message), record.PC)
	record.Attrs(func(a slog.Attr) bool {
		masked.AddAttrs(Attr(a))
		return true
	})
	return masked
}

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr that masks the registered
// credentials in a record's message, which it receives under slog.MessageKey,
// and in its attributes. It leaves the built-in time, level and source alone.
func ReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	// The built-in time, level and source are not user text. A user attribute
	// with one of these keys has another value type, so it is still masked.
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey:
			if a.Value.Kind() == slog.KindTime {
				return a
			}
		case slog.LevelKey:
			if _, ok := a.Value.Any().(slog.Level); ok {
				return a
			}
		case slog.SourceKey:
			if _, ok := a.Value.Any().(*slog.Source); ok {
				return a
			}
		}
	}
	// Masked here, before the handler escapes the value, so that a credential
	// holding a quote, a backslash or a newline is still found.
	return Attr(a)
}

// Attrs returns attrs with each value masked as Attr does.
func Attrs(attrs []slog.Attr) []slog.Attr {
	masked := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		masked[i] = Attr(a)
	}
	return masked
}

// Attr masks the registered credentials in a string attribute, in the
// attributes of a group, and in any other value's text: a byte slice's or what
// fmt.Sprint prints for an error, a Stringer, a slice, a map or a struct. Such a
// value is replaced by its masked text only when masking changed it, or when it
// is an error or a Stringer; every other value keeps its kind.
func Attr(a slog.Attr) slog.Attr {
	value := a.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Text(value.String()))
	case slog.KindGroup:
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(Attrs(value.Group())...)}
	case slog.KindAny:
		// fmt.Sprint, unlike a direct Error or String call, recovers from a
		// method that panics on a nil receiver, as the standard handlers do.
		var text string
		if b, ok := value.Any().([]byte); ok {
			text = string(b)
		} else {
			text = fmt.Sprint(value.Any())
		}
		masked := Text(text)
		// An error or a Stringer becomes its text even when nothing was masked:
		// a handler that calls its method itself, as the OpenTelemetry bridge does,
		// panics on a typed nil whose method reads its receiver, which fmt prints
		// as <nil>.
		_, isError := value.Any().(error)
		_, isStringer := value.Any().(fmt.Stringer)
		if masked != text || isError || isStringer {
			return slog.String(a.Key, masked)
		}
	}
	return slog.Attr{Key: a.Key, Value: value}
}
