// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package lokilog writes a Promtail-compatible targets file so that
// Promtail can discover and tail VM console logs with rich labels.
//
// nico-vms periodically scans for per-instance log files, queries
// the database for instance metadata, and writes a JSON targets file.
// Promtail watches this file via file_sd_configs and handles all the
// hard parts: tailing, offset tracking, rotation, and delivery to Loki.
package lokilog

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// Config controls the targets file writer.
type Config struct {
	// TargetsFile is the path to write the Promtail targets JSON.
	// Empty disables the writer.
	TargetsFile string

	// LogPathPrefix is the path prefix used in __path__ labels,
	// as seen by Promtail (which may differ from the host path
	// when Promtail runs in a container). Defaults to InstancesDir.
	LogPathPrefix string

	// RefreshInterval is how often to rescan and rewrite. Default 10s.
	RefreshInterval time.Duration
}

// logType defines a category of log files to discover per instance.
type logType struct {
	// Names are filenames to look for, in preference order.
	// The first match wins.
	Names []string
	// Type is the log_type label value.
	Type string
}

// knownLogTypes lists all per-instance log files to discover.
// Add new entries here to ship additional log types to Loki.
var knownLogTypes = []logType{
	{
		Names: []string{
			"console.log",          // darwin-qemu
			"disk.qcow2.ttyS0.log", // libvirt serial 0 (VM console)
			"disk.qcow2.ttyS1.log", // libvirt serial 1 (fallback)
		},
		Type: "console",
	},
	// Future examples:
	// {Names: []string{"agent.log"}, Type: "agent"},
	// {Names: []string{"kubelet.log"}, Type: "kubelet"},
}

// TargetsWriter periodically writes a Promtail file_sd_configs targets file.
type TargetsWriter struct {
	cfg          Config
	instancesDir string
	store        *state.Store
	logger       *slog.Logger
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// New creates a TargetsWriter. Call Start to begin the write loop.
func New(cfg Config, instancesDir string, store *state.Store, logger *slog.Logger) *TargetsWriter {
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 10 * time.Second
	}
	if cfg.LogPathPrefix == "" {
		cfg.LogPathPrefix = instancesDir
	}
	return &TargetsWriter{
		cfg:          cfg,
		instancesDir: instancesDir,
		store:        store,
		logger:       logger.With("component", "promtail-targets"),
	}
}

// Start begins the background write loop.
func (w *TargetsWriter) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(ctx)
	}()
	w.logger.Info("Promtail targets writer started",
		"file", w.cfg.TargetsFile,
		"interval", w.cfg.RefreshInterval,
	)
}

// Wait blocks until the writer goroutine exits.
func (w *TargetsWriter) Wait() {
	w.wg.Wait()
}

func (w *TargetsWriter) run(ctx context.Context) {
	// Write immediately on startup, then on interval.
	w.write(ctx)
	ticker := time.NewTicker(w.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.write(ctx)
		}
	}
}

// promtailTarget is the Promtail file_sd_configs JSON format.
type promtailTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

func (w *TargetsWriter) write(ctx context.Context) {
	logs := w.discoverLogs()
	meta := w.getInstanceMeta(ctx)

	var targets []promtailTarget
	for _, entry := range logs {
		labels := map[string]string{
			"job":         "vm_console",
			"log_type":    entry.logType,
			"instance_id": entry.instanceID,
			"__path__":    entry.promtailPath,
		}
		if m, ok := meta[entry.instanceID]; ok {
			if m.name != "" {
				labels["instance_name"] = m.name
			}
			if m.siteID != "" {
				labels["site_id"] = m.siteID
			}
			if m.status != "" {
				labels["status"] = m.status
			}
		}
		targets = append(targets, promtailTarget{
			Targets: []string{"localhost"},
			Labels:  labels,
		})
	}

	// Write empty array if no targets, so Promtail cleans up stale tailers.
	if targets == nil {
		targets = []promtailTarget{}
	}

	data, err := json.MarshalIndent(targets, "", "  ")
	if err != nil {
		w.logger.Error("Failed to marshal targets", "error", err)
		return
	}

	// Atomic write: tmp file + rename to avoid Promtail reading partial JSON.
	tmp := w.cfg.TargetsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		w.logger.Error("Failed to write targets file", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, w.cfg.TargetsFile); err != nil {
		w.logger.Error("Failed to rename targets file", "error", err)
	}
}

type discoveredLog struct {
	instanceID   string
	logType      string
	hostPath     string // actual path on disk
	promtailPath string // path as seen by Promtail
}

func (w *TargetsWriter) discoverLogs() []discoveredLog {
	entries, err := os.ReadDir(w.instancesDir)
	if err != nil {
		return nil
	}

	var results []discoveredLog
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		dir := filepath.Join(w.instancesDir, id)

		for _, lt := range knownLogTypes {
			for _, name := range lt.Names {
				hostPath := filepath.Join(dir, name)
				if info, err := os.Stat(hostPath); err == nil && info.Size() > 0 {
					promtailPath := filepath.Join(w.cfg.LogPathPrefix, id, name)
					results = append(results, discoveredLog{
						instanceID:   id,
						logType:      lt.Type,
						hostPath:     hostPath,
						promtailPath: promtailPath,
					})
					break // first match wins per log type
				}
			}
		}
	}
	return results
}

type instanceMeta struct {
	name   string
	siteID string
	status string
}

func (w *TargetsWriter) getInstanceMeta(ctx context.Context) map[string]instanceMeta {
	result := make(map[string]instanceMeta)
	rows, err := w.store.ListInstances(ctx, state.ListInstancesFilter{Backend: state.SiteBackendVM}, state.Page{Number: 1, Size: 1000})
	if err != nil {
		return result
	}
	for _, row := range rows {
		result[row.ID] = instanceMeta{
			name:   row.Name,
			siteID: row.SiteID,
			status: row.VM.ObservedState,
		}
	}
	return result
}

// CleanupTargetsFile removes the targets file, used by tests
// or when the instance directory is also being cleaned up.
func CleanupTargetsFile(path string) {
	os.Remove(path)
	os.Remove(path + ".tmp")
}

// ValidTargetsFile checks whether a targets file already exists at the given path,
// useful for log messages at startup.
func ValidTargetsFile(path string) bool {
	return strings.TrimSpace(path) != ""
}
