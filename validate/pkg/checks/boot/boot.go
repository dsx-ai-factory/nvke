// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package boot implements the boot-tier checks: behavioral assertions probed
// inside a booted throwaway copy of the image, for the facts a filesystem
// read cannot honestly establish — unit enablement as systemd resolves it,
// ordering, and runtime-applied sysctl values.
//
// Deferred (documented, not silently dropped): FDE-004 (grub key stripped
// after the TPM first-boot bind — needs the swtpm phase-2 harness).
package boot

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// Impls returns the boot-tier implementations keyed by check ID.
func Impls() map[string]checks.Func {
	return map[string]checks.Func{
		"SPEC-001":   checkAgentEnabled,
		"SPEC-002":   unitEnabledCheck("spire-agent.service"),
		"SPEC-003":   checkUnitOrdering,
		"SPEC-005":   checkConntrackApplied,
		"FDE-002":    unitEnabledCheck("luks-tpm-firstboot.service"),
		"COMPAT-003": checkBOMBooted,
	}
}

// Unit name generations: the nke→nvke rename (#1548, #1565) lands after
// v0.2.0, so both spellings identify valid NVKE images. Name-sensitive checks
// resolve which generation the booted image carries before asserting.
var (
	agentUnits          = []string{"nvke-agent.service", "nke-agent.service"}
	spireConfigureUnits = []string{"nvke-spire-configure.service", "nke-spire-configure.service"}
)

// resolveUnit returns the first candidate systemd reports loaded. When none
// is, it returns "" plus the observed load states for the failure evidence.
func resolveUnit(ctx context.Context, in checks.Inputs, candidates []string) (string, string, error) {
	var states []string
	for _, unit := range candidates {
		res, err := in.Boot.Probe(ctx, "systemctl show -p LoadState --value "+unit)
		if err != nil {
			return "", "", err
		}
		state := strings.TrimSpace(res.Stdout)
		if state == "loaded" {
			return unit, "", nil
		}
		if state == "" {
			state = fmt.Sprintf("exit %d", res.ExitCode)
		}
		states = append(states, unit+" "+state)
	}
	return "", strings.Join(states, ", "), nil
}

// checkAgentEnabled (SPEC-001): the node agent unit — whichever name
// generation the image carries — is enabled as systemd resolves it.
func checkAgentEnabled(ctx context.Context, t checks.Target, in checks.Inputs) (checks.Outcome, error) {
	unit, states, err := resolveUnit(ctx, in, agentUnits)
	if err != nil {
		return checks.Outcome{}, err
	}
	if unit == "" {
		return checks.Outcome{Status: checks.StatusFail, Evidence: "no node agent unit is present: " + states}, nil
	}
	return unitEnabledCheck(unit)(ctx, t, in)
}

// unitEnabledCheck builds a check that systemd reports the unit enabled —
// the unit's own answer, including drop-ins and aliases, which a symlink read
// can approximate but not settle.
func unitEnabledCheck(unit string) checks.Func {
	return func(ctx context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
		res, err := in.Boot.Probe(ctx, "systemctl is-enabled "+unit)
		if err != nil {
			return checks.Outcome{}, err
		}
		state := strings.TrimSpace(res.Stdout)
		if res.ExitCode != 0 || state != "enabled" {
			if state == "" {
				state = fmt.Sprintf("exit %d", res.ExitCode)
			}
			return checks.Outcome{Status: checks.StatusFail, Evidence: unit + " is " + state + ", want enabled"}, nil
		}
		return checks.Outcome{Status: checks.StatusPass, Evidence: unit + " enabled"}, nil
	}
}

// checkUnitOrdering (SPEC-003): the SPIRE configure unit runs before
// spire-agent, and spire-agent before the node agent — asked of systemd's
// resolved dependency graph, where image-owned drop-ins have already been
// merged.
func checkUnitOrdering(ctx context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	agent, agentStates, err := resolveUnit(ctx, in, agentUnits)
	if err != nil {
		return checks.Outcome{}, err
	}
	if agent == "" {
		return checks.Outcome{Status: checks.StatusFail, Evidence: "no node agent unit is present: " + agentStates}, nil
	}
	configure, configureStates, err := resolveUnit(ctx, in, spireConfigureUnits)
	if err != nil {
		return checks.Outcome{}, err
	}
	if configure == "" {
		return checks.Outcome{Status: checks.StatusFail, Evidence: "no SPIRE configure unit is present: " + configureStates}, nil
	}
	pairs := []struct{ earlier, later string }{
		{configure, "spire-agent.service"},
		{"spire-agent.service", agent},
	}
	for _, p := range pairs {
		res, err := in.Boot.Probe(ctx, "systemctl show -p After --value "+p.later)
		if err != nil {
			return checks.Outcome{}, err
		}
		if res.ExitCode != 0 {
			return checks.Outcome{Status: checks.StatusFail, Evidence: fmt.Sprintf("systemctl show %s: exit %d", p.later, res.ExitCode)}, nil
		}
		if !containsUnit(res.Stdout, p.earlier) {
			return checks.Outcome{
				Status:   checks.StatusFail,
				Evidence: fmt.Sprintf("%s does not order After=%s", p.later, p.earlier),
			}, nil
		}
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("%s < spire-agent.service < %s ordering holds", configure, agent),
	}, nil
}

// checkBOMBooted (COMPAT-003): the booted image's package database reports
// every target-BOM component installed at the required version. COMPAT-002
// checks the release's paperwork (the attested SBOM); this asks the running
// system, so together they close the chain target BOM → SBOM → booted image.
// BOM names are charset-validated at parse time (static.ParseBOM), so
// embedding them in the probe command is safe.
func checkBOMBooted(ctx context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	if in.BOM == nil {
		return checks.Outcome{
			Status:   checks.StatusSkip,
			Reason:   checks.SkipInputMissing,
			Evidence: "no target BOM supplied (--target-bom)",
		}, nil
	}
	names := make([]string, 0, len(in.BOM))
	for name := range in.BOM {
		names = append(names, name)
	}
	sort.Strings(names)
	var drift []string
	for _, name := range names {
		res, err := in.Boot.Probe(ctx, "dpkg-query -W -f='${db:Status-Status} ${Version}' "+name)
		if err != nil {
			return checks.Outcome{}, err
		}
		if res.ExitCode != 0 {
			drift = append(drift, name+": not installed")
			continue
		}
		state, version, _ := strings.Cut(strings.TrimSpace(res.Stdout), " ")
		switch {
		case state != "installed":
			drift = append(drift, fmt.Sprintf("%s: dpkg state %s, want installed", name, state))
		case version != in.BOM[name]:
			drift = append(drift, fmt.Sprintf("%s: bom %s, booted image %s", name, in.BOM[name], version))
		}
	}
	if len(drift) > 0 {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("%d component(s) off the target BOM: %s", len(drift), strings.Join(drift, "; ")),
		}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("all %d BOM components installed at required versions in the booted image", len(in.BOM)),
	}, nil
}

func containsUnit(afterList, unit string) bool {
	for _, u := range strings.Fields(afterList) {
		if u == unit {
			return true
		}
	}
	return false
}

// checkConntrackApplied (SPEC-005): the image's conntrack tuning is live in
// the booted kernel — TIME_WAIT at 60s (the sysctl.d file) and nf_conntrack
// loaded so the key exists at all. The runtime value is the point: the files'
// presence is necessary but a boot-time ordering defect would leave the
// kernel default applied, and only asking the kernel catches that.
func checkConntrackApplied(ctx context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	res, err := in.Boot.Probe(ctx, "sysctl -n net.netfilter.nf_conntrack_tcp_timeout_time_wait")
	if err != nil {
		return checks.Outcome{}, err
	}
	if res.ExitCode != 0 {
		return checks.Outcome{Status: checks.StatusFail, Evidence: "nf_conntrack is not loaded: sysctl exit " + fmt.Sprint(res.ExitCode)}, nil
	}
	value := strings.TrimSpace(res.Stdout)
	if value != "60" {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: "nf_conntrack_tcp_timeout_time_wait is " + value + ", want 60 (10-nke-conntrack-timeouts.conf not applied)",
		}, nil
	}
	return checks.Outcome{Status: checks.StatusPass, Evidence: "conntrack TIME_WAIT timeout applied (60s)"}, nil
}
