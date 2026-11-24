// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The state directory (CTRD_ROOT) holds what containerd does not:
//
//	lock                       serializes changes to the files below
//	containers/<id>/meta.json  the container as it was created, and its last attachment
//	containers/<id>/{hosts,resolv.conf,hostname}  bind-mounted over /etc in the container
//	containers/<id>/log        the console, written by the supervisor
//	containers/<id>/exitcode   the exit status of the last run
//	containers/<id>/volumes/N  anonymous volumes
//	networks/<name>.json       a network, and networks/<name>.conflist its CNI configuration
//	ipam/<name>/               host-local's leases
//
// Removing a container removes its directory.

type portMapping struct {
	HostIP        string `json:"HostIP"`
	HostPort      int    `json:"HostPort"`
	ContainerPort int    `json:"ContainerPort"`
	Protocol      string `json:"Protocol"`
}

type volume struct {
	Source      string   `json:"Source"`
	Destination string   `json:"Destination"`
	Options     []string `json:"Options"`
	Anonymous   bool     `json:"Anonymous"`
}

// containerMeta is a container: what create was asked for and what the last start attached it to.
type containerMeta struct {
	ID         string            `json:"ID"`
	Name       string            `json:"Name"`
	Image      string            `json:"Image"`
	Hostname   string            `json:"Hostname"`
	Created    time.Time         `json:"Created"`
	Labels     map[string]string `json:"Labels"`
	Env        []string          `json:"Env"`
	Entrypoint []string          `json:"Entrypoint"`
	Command    []string          `json:"Command"`
	// Network is the network name, or "host".
	Network    string            `json:"Network"`
	Publish    []portMapping     `json:"Publish"`
	Sysctls    map[string]string `json:"Sysctls"`
	Privileged bool              `json:"Privileged"`
	Tty        bool              `json:"Tty"`
	Restart    string            `json:"Restart"`
	CapAdd     []string          `json:"CapAdd"`
	GPUs       []string          `json:"GPUs"`
	Volumes    []volume          `json:"Volumes"`
	Tmpfs      []volume          `json:"Tmpfs"`

	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
	ExitCode   int       `json:"ExitCode"`

	// Attachment is set while the container is attached to its network.
	IPv4 string `json:"IPv4"`
	IPv6 string `json:"IPv6"`
	MAC  string `json:"MAC"`
	// LastAddresses are the addresses of the last attachment, asked for again by the next.
	LastAddresses []string `json:"LastAddresses"`
	// CNIResult is what the plugins returned, which a detach hands back to them.
	CNIResult json.RawMessage `json:"CNIResult,omitempty"`

	// ResolvConf is the /etc/resolv.conf the container was created with, which every start writes again,
	// as Docker fixes --dns at create. It is empty on a container created before it was recorded, and
	// on a host-network one, which uses the host's file.
	ResolvConf string `json:"ResolvConf,omitempty"`
}

func (e *engine) containersDir() string { return filepath.Join(e.cfg.Root, "containers") }

func (e *engine) containerDir(id string) string { return filepath.Join(e.containersDir(), id) }

// withLock runs fn holding the state lock.
func (e *engine) withLock(fn func() error) error {
	if err := os.MkdirAll(e.cfg.Root, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(e.cfg.Root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking %s: %w", lock.Name(), err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// writeJSON writes v to path through a rename, so a reader never sees half of it.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { //nolint:gosec // the engine writes its own state files
		return err
	}
	return os.Rename(tmp, path)
}

func (e *engine) saveContainer(meta *containerMeta) error {
	return writeJSON(filepath.Join(e.containerDir(meta.ID), "meta.json"), meta)
}

func (e *engine) loadContainer(id string) (*containerMeta, error) {
	data, err := os.ReadFile(filepath.Join(e.containerDir(id), "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta containerMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("reading the state of container %s: %w", id, err)
	}
	return &meta, nil
}

// listContainers returns every container, oldest first.
func (e *engine) listContainers() ([]*containerMeta, error) {
	entries, err := os.ReadDir(e.containersDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var all []*containerMeta
	for _, entry := range entries {
		// A container being created has a directory and no meta.json yet.
		if meta, err := e.loadContainer(entry.Name()); err == nil {
			all = append(all, meta)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Created.Before(all[j].Created) })
	return all, nil
}

// findContainer resolves a name, an ID or the unique prefix of one.
func (e *engine) findContainer(ref string) (*containerMeta, error) {
	all, err := e.listContainers()
	if err != nil {
		return nil, err
	}
	var prefixed []*containerMeta
	for _, meta := range all {
		if meta.Name == ref || meta.ID == ref {
			return meta, nil
		}
		if ref != "" && strings.HasPrefix(meta.ID, ref) {
			prefixed = append(prefixed, meta)
		}
	}
	switch len(prefixed) {
	case 0:
		return nil, fmt.Errorf("no such container: %s", ref)
	case 1:
		return prefixed[0], nil
	default:
		return nil, fmt.Errorf("multiple containers match %q", ref)
	}
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
