// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	nethttp "net/http"
	"slices"
	"sync"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
)

// noMachinesAvailable is NICo's create rejection when no machine matches the
// request; CAPNICo reads it on a domain-pinned create as FailureDomainUnavailable.
const noMachinesAvailable = "No Machines are available for specified Instance Type"

// FailureDomainConfig is the failure-domain layout of the simulated machine
// inventory, set at runtime through /sim/failure-domains. The zero value models
// no domains: the machine list is empty and label selectors are ignored.
type FailureDomainConfig struct {
	LabelKey string   `json:"labelKey"`
	Domains  []string `json:"domains"`
	// Unavailable domains reject every create pinned to them.
	Unavailable []string `json:"unavailable"`
}

func (c FailureDomainConfig) validate() error {
	if len(c.Domains) > 0 && c.LabelKey == "" {
		return errors.New("labelKey is required when domains are set")
	}
	for i, domain := range c.Domains {
		if domain == "" || slices.Contains(c.Domains[:i], domain) {
			return fmt.Errorf("domains must be unique and non-empty, got %q", c.Domains)
		}
	}
	for _, domain := range c.Unavailable {
		if !slices.Contains(c.Domains, domain) {
			return fmt.Errorf("unavailable domain %q is not one of domains", domain)
		}
	}
	return nil
}

// failureDomains holds the runtime failure-domain layout and the machine labels
// each domain-pinned instance landed on. Neither survives a restart.
type failureDomains struct {
	mu     sync.Mutex
	config FailureDomainConfig
	labels map[string]map[string]string
}

func (f *failureDomains) set(config FailureDomainConfig) error {
	if err := config.validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config = FailureDomainConfig{
		LabelKey:    config.LabelKey,
		Domains:     slices.Clone(config.Domains),
		Unavailable: slices.Clone(config.Unavailable),
	}
	return nil
}

func (f *failureDomains) get() FailureDomainConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FailureDomainConfig{
		LabelKey:    f.config.LabelKey,
		Domains:     slices.Clone(f.config.Domains),
		Unavailable: slices.Clone(f.config.Unavailable),
	}
}

func (f *failureDomains) machines(siteID string) []nicosdk.Machine {
	config := f.get()
	out := make([]nicosdk.Machine, 0, len(config.Domains))
	for _, domain := range config.Domains {
		machine := nicosdk.NewMachine()
		machine.SetId("fd-machine-" + domain)
		machine.SetSiteId(siteID)
		machine.SetLabels(map[string]string{config.LabelKey: domain})
		out = append(out, *machine)
	}
	return out
}

// admit checks a create's machine label selector against the configured
// domains and returns the labels of the machine the instance lands on. It
// reports false when the selector matches no available domain, and returns nil
// labels for an unpinned create or while no domains are configured.
func (f *failureDomains) admit(selector map[string]string) (map[string]string, bool) {
	config := f.get()
	if len(selector) == 0 || len(config.Domains) == 0 {
		return nil, true
	}
	domain, ok := selector[config.LabelKey]
	if !ok || len(selector) != 1 || !slices.Contains(config.Domains, domain) || slices.Contains(config.Unavailable, domain) {
		return nil, false
	}
	return map[string]string{config.LabelKey: domain}, true
}

func (f *failureDomains) record(instanceID string, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labels == nil {
		f.labels = map[string]map[string]string{}
	}
	f.labels[instanceID] = labels
}

func (f *failureDomains) machineLabels(instanceID string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.labels[instanceID])
}

func (f *failureDomains) forget(instanceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.labels, instanceID)
}

func (s *Server) handleListMachines(w nethttp.ResponseWriter, r *nethttp.Request) {
	if parsePage(r).Number > 1 {
		writeJSON(w, nethttp.StatusOK, []nicosdk.Machine{})
		return
	}
	writeJSON(w, nethttp.StatusOK, s.failureDomains.machines(r.URL.Query().Get("siteId")))
}

func (s *Server) handleSimGetFailureDomains(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, nethttp.StatusOK, s.failureDomains.get())
}

func (s *Server) handleSimPutFailureDomains(w nethttp.ResponseWriter, r *nethttp.Request) {
	r.Body = nethttp.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var config FailureDomainConfig
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		writeForgeError(w, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.failureDomains.set(config); err != nil {
		writeForgeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("failure domains configured", "labelKey", config.LabelKey, "domains", config.Domains, "unavailable", config.Unavailable)
	writeJSON(w, nethttp.StatusOK, s.failureDomains.get())
}
