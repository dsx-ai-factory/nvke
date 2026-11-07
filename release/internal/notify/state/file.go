// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package state keeps the Slack thread a run's messages belong to.
//
// A Slack thread is the ts of its first message, so the whole state is one
// short string per conversation and channel. It is stored as a file so a CI
// cache can carry it between jobs and between workflow runs.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// tsPattern is the shape of a Slack message timestamp.
var tsPattern = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,9}$`)

// File stores thread timestamps as files under Dir.
type File struct {
	Dir string
}

// Get returns the thread timestamp for a conversation in a channel, or "" when
// there is none. A missing file and a file that is not a timestamp both mean
// none: state comes back from a cache that can be evicted or truncated, and the
// answer to that is a new thread, not a failed notification.
func (f File) Get(key, channel string) (string, error) {
	data, err := os.ReadFile(f.path(key, channel))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading thread state: %w", err)
	}
	ts := strings.TrimSpace(string(data))
	if !tsPattern.MatchString(ts) {
		return "", nil
	}
	return ts, nil
}

// Set records the thread timestamp for a conversation in a channel.
func (f File) Set(key, channel, ts string) error {
	if !tsPattern.MatchString(ts) {
		return fmt.Errorf("not a Slack timestamp: %q", ts)
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}

	// Written beside the target and renamed over it, so a reader never sees a
	// half-written timestamp.
	tmp, err := os.CreateTemp(f.Dir, ".thread-*")
	if err != nil {
		return fmt.Errorf("writing thread state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(ts + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("writing thread state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing thread state: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("writing thread state: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.path(key, channel)); err != nil {
		return fmt.Errorf("writing thread state: %w", err)
	}
	return nil
}

// path is the file for a conversation in a channel. Both parts are escaped, so
// a key that names a directory or carries a separator stays inside Dir, and the
// "~" that joins them is never produced by the escaping.
func (f File) path(key, channel string) string {
	return filepath.Join(f.Dir, "thread-"+escape(key)+"~"+escape(channel)+".ts")
}

// escape keeps letters, digits and . _ - and writes every other byte as %XX.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
