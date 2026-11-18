// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dpkg reads the dpkg status database of an image root or of the
// running node. It reports every package dpkg records, whatever its state, so a
// held, unpacked or half-configured package is never mistaken for an absent one.
package dpkg

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// StatusPath is the dpkg status database, relative to a root.
const StatusPath = "var/lib/dpkg/status"

var (
	// ErrEmpty reports a status database that holds no package stanza.
	ErrEmpty = errors.New("dpkg status database is empty")
	// ErrMalformed reports a status database that dpkg itself would reject.
	ErrMalformed = errors.New("malformed dpkg status database")
)

// Want is the first word of a Status field: what the administrator asked for.
type Want string

// Want values, as dpkg writes them.
const (
	WantUnknown   Want = "unknown"
	WantInstall   Want = "install"
	WantHold      Want = "hold"
	WantDeinstall Want = "deinstall"
	WantPurge     Want = "purge"
)

// Flag is the second word of a Status field.
type Flag string

// Flag values, as dpkg writes them.
const (
	FlagOK Flag = "ok"
	// FlagReinstReq marks a package dpkg must reinstall before it can remove it.
	FlagReinstReq Flag = "reinstreq"
)

// State is the third word of a Status field: where the package actually is.
type State string

// State values, as dpkg writes them.
const (
	StateNotInstalled    State = "not-installed"
	StateConfigFiles     State = "config-files"
	StateHalfInstalled   State = "half-installed"
	StateUnpacked        State = "unpacked"
	StateHalfConfigured  State = "half-configured"
	StateTriggersAwaited State = "triggers-awaited"
	StateTriggersPending State = "triggers-pending"
	StateInstalled       State = "installed"
)

var (
	wants  = []Want{WantUnknown, WantInstall, WantHold, WantDeinstall, WantPurge}
	flags  = []Flag{FlagOK, FlagReinstReq}
	states = []State{
		StateNotInstalled, StateConfigFiles, StateHalfInstalled, StateUnpacked,
		StateHalfConfigured, StateTriggersAwaited, StateTriggersPending, StateInstalled,
	}
)

// Status is a package's Status field, split into dpkg's want, flag and state.
type Status struct {
	Want  Want  `json:"want"`
	Flag  Flag  `json:"flag"`
	State State `json:"state"`
}

// String returns the field as dpkg writes it, e.g. "hold ok installed".
func (s Status) String() string {
	return string(s.Want) + " " + string(s.Flag) + " " + string(s.State)
}

// Package is one stanza of the status database. Source and SourceVersion are
// empty when the binary package carries no Source field; Version and
// Architecture may be empty for a not-installed or half-installed package, and
// Architecture for a config-files one.
type Package struct {
	Name          string `json:"name"`
	Version       string `json:"version,omitempty"`
	Architecture  string `json:"architecture,omitempty"`
	Source        string `json:"source,omitempty"`
	SourceVersion string `json:"sourceVersion,omitempty"`
	Status        Status `json:"status"`
}

// Held reports whether the package is on hold, whatever its state.
func (p Package) Held() bool { return p.Status.Want == WantHold }

// Installed reports whether the package is fully installed and configured:
// state installed with no reinstall required. A held package can be installed.
func (p Package) Installed() bool {
	return p.Status.State == StateInstalled && p.Status.Flag == FlagOK
}

// Manifest is every package in a status database, sorted by name and then
// architecture. It is the data a manifest verb prints.
type Manifest struct {
	Packages []Package `json:"packages"`
}

// ReadRoot reads the status database under root; "/" reads the running node.
func ReadRoot(root string) (Manifest, error) {
	m, err := ReadFS(os.DirFS(root))
	if err != nil {
		return Manifest{}, fmt.Errorf("reading %s: %w", filepath.Join(root, StatusPath), err)
	}
	return m, nil
}

// ReadFS reads the status database at StatusPath in fsys.
func ReadFS(fsys fs.FS) (Manifest, error) {
	f, err := fsys.Open(StatusPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("opening dpkg status: %w", err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads a status database. Every stanza is reported, including
// not-installed ones that dpkg keeps for a selection or a hold: callers that
// want only what is on disk filter on State. Field names are matched without
// regard to case. A stanza without Package or Status, a field repeated in one
// stanza, an unknown Status word, a duplicate name and architecture, or a
// database with no stanza at all is an error.
func Parse(r io.Reader) (Manifest, error) {
	br := bufio.NewReader(r)
	var (
		pkgs   []Package
		fields map[string]string
		last   string
		start  int
		lineNo int
	)
	flush := func() error {
		if fields == nil {
			return nil
		}
		p, err := newPackage(fields)
		if err != nil {
			return fmt.Errorf("%w: stanza at line %d: %w", ErrMalformed, start, err)
		}
		pkgs = append(pkgs, p)
		fields, last = nil, ""
		return nil
	}
	for {
		line, readErr := br.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Manifest{}, fmt.Errorf("reading dpkg status: %w", readErr)
		}
		if line != "" {
			lineNo++
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.TrimSpace(line) == "":
			if err := flush(); err != nil {
				return Manifest{}, err
			}
		case line[0] == ' ' || line[0] == '\t':
			// Continuation lines belong to the previous field; only the
			// one-line fields this package reports are kept.
			if last == "" {
				return Manifest{}, fmt.Errorf("%w: line %d: continuation line with no field", ErrMalformed, lineNo)
			}
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok || name == "" {
				return Manifest{}, fmt.Errorf("%w: line %d: %q is not a field", ErrMalformed, lineNo, line)
			}
			if fields == nil {
				fields, start = map[string]string{}, lineNo
			}
			// dpkg field names are case-insensitive, and dpkg rejects a field repeated in one stanza.
			name = strings.ToLower(name)
			if _, dup := fields[name]; dup {
				return Manifest{}, fmt.Errorf("%w: line %d: field %q repeated in one stanza", ErrMalformed, lineNo, name)
			}
			last = name
			fields[name] = strings.TrimSpace(value)
		}
		if readErr != nil {
			break
		}
	}
	if err := flush(); err != nil {
		return Manifest{}, err
	}
	if len(pkgs) == 0 {
		return Manifest{}, ErrEmpty
	}
	slices.SortFunc(pkgs, func(a, b Package) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Architecture, b.Architecture))
	})
	for i := 1; i < len(pkgs); i++ {
		if pkgs[i].Name == pkgs[i-1].Name && pkgs[i].Architecture == pkgs[i-1].Architecture {
			return Manifest{}, fmt.Errorf("%w: package %s:%s appears twice", ErrMalformed, pkgs[i].Name, pkgs[i].Architecture)
		}
	}
	return Manifest{Packages: pkgs}, nil
}

// newPackage builds a Package from one stanza's fields.
func newPackage(fields map[string]string) (Package, error) {
	p := Package{
		Name:         fields["package"],
		Version:      fields["version"],
		Architecture: fields["architecture"],
	}
	if p.Name == "" {
		return Package{}, errors.New("no Package field")
	}
	raw, ok := fields["status"]
	if !ok {
		return Package{}, fmt.Errorf("package %s has no Status field", p.Name)
	}
	status, err := parseStatus(raw)
	if err != nil {
		return Package{}, fmt.Errorf("package %s: %w", p.Name, err)
	}
	p.Status = status
	// As dpkg does: a not-installed or half-installed record may lack Version,
	// and a config-files one written by an old dpkg may also lack Architecture.
	exempt := status.State == StateNotInstalled || status.State == StateHalfInstalled
	if !exempt && p.Version == "" {
		return Package{}, fmt.Errorf("package %s is %s but lacks Version", p.Name, status.State)
	}
	if !exempt && status.State != StateConfigFiles && p.Architecture == "" {
		return Package{}, fmt.Errorf("package %s is %s but lacks Architecture", p.Name, status.State)
	}
	// Source is "name" or "name (version)"; without a version the source
	// version is the binary's.
	if src := fields["source"]; src != "" {
		p.Source, p.SourceVersion = src, p.Version
		if open := strings.IndexByte(src, '('); open > 0 && strings.HasSuffix(src, ")") {
			p.Source = strings.TrimSpace(src[:open])
			p.SourceVersion = strings.TrimSpace(src[open+1 : len(src)-1])
		}
	}
	return p, nil
}

// parseStatus splits a Status field into its three words and checks each
// against the values dpkg writes.
func parseStatus(raw string) (Status, error) {
	words := strings.Fields(raw)
	if len(words) != 3 {
		return Status{}, fmt.Errorf("status %q is not three words", raw)
	}
	s := Status{Want: Want(words[0]), Flag: Flag(words[1]), State: State(words[2])}
	if !slices.Contains(wants, s.Want) {
		return Status{}, fmt.Errorf("status %q: unknown want %q", raw, s.Want)
	}
	if !slices.Contains(flags, s.Flag) {
		return Status{}, fmt.Errorf("status %q: unknown flag %q", raw, s.Flag)
	}
	if !slices.Contains(states, s.State) {
		return Status{}, fmt.Errorf("status %q: unknown state %q", raw, s.State)
	}
	return s, nil
}
