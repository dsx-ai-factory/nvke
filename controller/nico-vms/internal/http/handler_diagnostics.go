// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	nethttp "net/http"

	"github.com/NVIDIA/nke/controller/nico-vms/internal/sshexec"
	"github.com/NVIDIA/nke/controller/nico-vms/internal/state"
)

// CatalogItem defines a retrievable diagnostic item from a VM.
type CatalogItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`   // "log" or "file"
	Syntax    string `json:"syntax"` // for files: "json", "yaml", "shell", etc.
	Follow    bool   `json:"follow"` // whether the item supports follow/streaming
	cmd       string // one-shot command
	followCmd string // streaming command (empty = not followable)
}

// catalog is the predefined list of diagnostic items.
// Add new entries here to expose additional VM files/logs.
var catalog = []CatalogItem{
	{
		ID: "agent-logs", Name: "Node Agent Logs", Type: "log", Follow: true,
		cmd:       "sudo journalctl -b -u nke-agent --no-pager -n 500 -o cat",
		followCmd: "sudo journalctl -b -u nke-agent -f -o cat",
	},
	{
		ID: "kubelet-logs", Name: "Kubelet Logs", Type: "log", Follow: true,
		cmd:       "sudo journalctl -b -u kubelet --no-pager -n 500 -o cat",
		followCmd: "sudo journalctl -b -u kubelet -f -o cat",
	},
	{
		ID: "containerd-logs", Name: "Containerd Logs", Type: "log", Follow: true,
		cmd:       "sudo journalctl -b -u containerd --no-pager -n 500 -o cat",
		followCmd: "sudo journalctl -b -u containerd -f -o cat",
	},
	{
		ID: "cloud-init-logs", Name: "Cloud-Init Logs", Type: "log", Follow: false,
		cmd: "sudo journalctl -b -u cloud-init --no-pager -o cat",
	},
	{
		ID: "spire-agent-logs", Name: "SPIRE Agent Logs", Type: "log", Follow: true,
		cmd:       "sudo journalctl -b -u spire-agent --no-pager -n 500 -o cat",
		followCmd: "sudo journalctl -b -u spire-agent -f -o cat",
	},
	{
		ID: "agent-state", Name: "Agent State", Type: "file", Syntax: "json",
		cmd: "sudo cat /var/lib/nke/state.json",
	},
	{
		ID: "kubelet-config", Name: "Kubelet Config", Type: "file", Syntax: "yaml",
		cmd: "cat /var/lib/kubelet/config.yaml",
	},
}

var catalogByID = func() map[string]CatalogItem {
	m := make(map[string]CatalogItem, len(catalog))
	for _, item := range catalog {
		m[item.ID] = item
	}
	return m
}()

func (s *Server) handleListDiagnostics(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, nethttp.StatusOK, catalog)
}

func (s *Server) handleGetDiagnostic(w nethttp.ResponseWriter, r *nethttp.Request) {
	instanceID := r.PathValue("instanceId")
	itemID := r.PathValue("itemId")

	item, ok := catalogByID[itemID]
	if !ok {
		nethttp.Error(w, "unknown diagnostic item", nethttp.StatusNotFound)
		return
	}

	// Resolve the VM's IP address from the store.
	ip, err := s.resolveInstanceIP(r.Context(), instanceID)
	if err != nil {
		nethttp.Error(w, "instance not found or has no IP", nethttp.StatusNotFound)
		return
	}

	follow := r.URL.Query().Get("follow") == "true" && item.followCmd != ""

	if follow {
		s.streamDiagnostic(w, r, ip, item)
	} else {
		s.execDiagnostic(w, r, ip, item)
	}
}

func (s *Server) execDiagnostic(w nethttp.ResponseWriter, r *nethttp.Request, ip string, item CatalogItem) {
	output, err := sshexec.Exec(r.Context(), ip, s.sshConfig, item.cmd)
	if err != nil {
		s.logger.Error("diagnostic exec failed", "item", item.ID, "ip", ip, "error", err)
		nethttp.Error(w, "command failed: "+err.Error(), nethttp.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(output)
}

func (s *Server) streamDiagnostic(w nethttp.ResponseWriter, r *nethttp.Request, ip string, item CatalogItem) {
	flusher, ok := w.(nethttp.Flusher)
	if !ok {
		nethttp.Error(w, "streaming not supported", nethttp.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	err := sshexec.ExecStream(r.Context(), ip, s.sshConfig, item.followCmd, w, flusher.Flush)
	if err != nil && r.Context().Err() == nil {
		s.logger.Error("diagnostic stream failed", "item", item.ID, "ip", ip, "error", err)
	}
}

// resolveInstanceIP looks up the first IP address for the given instance.
func (s *Server) resolveInstanceIP(ctx context.Context, instanceID string) (string, error) {
	rows, err := s.store.ListInstances(ctx, state.ListInstancesFilter{}, state.Page{Number: 1, Size: 1000})
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Instance.ID != instanceID {
			continue
		}
		if row.Instance.ObservedState != "ready" && row.Instance.ObservedState != "rebooting" {
			return "", nethttp.ErrNotSupported
		}
		for _, iface := range row.Interfaces {
			ips, _ := iface.IPAddresses()
			if len(ips) > 0 {
				return ips[0], nil
			}
		}
	}
	return "", nethttp.ErrNotSupported
}
