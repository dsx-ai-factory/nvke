// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"strings"
	"testing"
)

const shipped = "../../triage.yaml"

// log joins lines as a runner prints them: a timestamp before each.
func log(lines ...string) string {
	var b strings.Builder

	for _, l := range lines {
		b.WriteString("2026-11-16T10:00:00.0000000Z ")
		b.WriteString(l)
		b.WriteByte('\n')
	}

	return b.String()
}

const exit1 = "##[error]Process completed with exit code 1."

func TestClassify(t *testing.T) {
	cfg, err := Load(shipped)
	if err != nil {
		t.Fatalf("the shipped rules do not load: %v", err)
	}

	tests := []struct {
		name       string
		step       string
		log        string
		wantRule   string
		wantClass  string
		wantAction Action
		wantInEvid string
	}{
		{
			name: "os image not published",
			step: "Check every artifact the plan names",
			log: log(
				"Error: 1 of the 8 artifacts the plan names were still not published after 1h30m0s:",
				"  https://cdn.example/nvke-packages/os-image/releases/v0.24.0-beta.8/amd64/node-amd64.qcow2 (os-image-amd64)",
				"    could not confirm https://cdn.example/nvke-packages/os-image/releases/v0.24.0-beta.8/amd64/node-amd64.qcow2 exists: HTTP 403. A missing artifact",
				exit1,
			),
			wantRule:   "os-image-not-published",
			wantClass:  "os-image-not-published",
			wantAction: ActionRerunImageBuild,
			wantInEvid: "exists: HTTP 403",
		},
		{
			name: "controller image not on the registry",
			step: "Check every artifact the plan names",
			log: log(
				"  oci://registry.example/org/nvke-controller:v0.24.0-beta.10 (controller)",
				"    registry.example/org/nvke-controller:v0.24.0-beta.10 is not published: the registry has no v0.24.0-beta.10 under org/nvke-controller",
				exit1,
			),
			wantRule:   "registry-image-not-published",
			wantClass:  "registry-image-not-published",
			wantAction: ActionNone,
			wantInEvid: "is not published",
		},
		{
			name:       "resolved inputs row missing",
			step:       "Record the resolved inputs",
			log:        log("##[error]the resolved-inputs table does not cover inputs.notify; add a row", exit1),
			wantRule:   "resolved-inputs-row-missing",
			wantClass:  "resolved-inputs-row-missing",
			wantAction: ActionNone,
			wantInEvid: "inputs.notify",
		},
		{
			name:       "plan dependency constraint",
			step:       "Plan",
			log:        log("Error: resolving cni-ovn for nvke: 1.5.18 does not match the constraint 2.4", exit1),
			wantRule:   "plan-dependency-constraint",
			wantClass:  "plan-dependency-constraint",
			wantAction: ActionNone,
			wantInEvid: "cni-ovn",
		},
		{
			name:       "teleport identity",
			step:       "Start the Teleport identity daemon",
			log:        log("##[error]tbot wrote no identity in 60s; the run cannot reach the jump host", exit1),
			wantRule:   "teleport-identity-missing",
			wantClass:  "teleport-identity-missing",
			wantAction: ActionNone,
			wantInEvid: "tbot wrote no identity",
		},
		{
			name: "site inputs stale",
			step: "Check the staged inputs are current",
			log: log(
				"##[error]the downloaded site inputs were validated at attempt 1, not this attempt (1); qualify-validate ran again without the artifact being refreshed",
				exit1,
			),
			wantRule:   "site-inputs-stale",
			wantClass:  "site-inputs-stale",
			wantAction: ActionNone,
			wantInEvid: "validated at attempt 1",
		},
		{
			name: "forge capacity",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] in [BeforeAll] - /work/controller/test/e2e/site_controller_test.go:226 @ 10/28/26 15:12:53.683",
				"• [FAILED] [0.694 seconds]",
				"Site controller [BeforeAll] provisions a node",
				"",
				"  [FAILED] Expected success, but got an error:",
				"      <*errors.errorString | 0x143041b9a160>: ",
				"      forge has no usable allocation for this run: cpu.arm (id): need 2, unusedUsable=1 (total=7 used=6 unused=1)",
				"  In [BeforeAll] at: /work/controller/test/e2e/site_controller_test.go:226",
				exit1,
			),
			wantRule:   "forge-no-usable-allocation",
			wantClass:  "forge-capacity",
			wantAction: ActionWaitThenRerun,
			wantInEvid: "need 2, unusedUsable=1",
		},
		{
			name: "forge allocation forbidden",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] Expected success, but got an error:",
				"      <*fmt.wrapError | 0x1a6cbc084100>: ",
				`      forge: reading allocation stats for cpu.arm (id): 403 Forbidden: {"requestStatus":{"statusCode":"FORBIDDEN"}}`,
				exit1,
			),
			wantRule:   "forge-allocation-forbidden",
			wantClass:  "forge-access",
			wantAction: ActionNone,
			wantInEvid: "403 Forbidden",
		},
		{
			name: "control plane never joined",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] in [It] - /work/controller/test/e2e/tenant_test.go:564 @ 11/07/26 03:36:05.25",
				"• [FAILED] [1596.869 seconds]",
				"",
				"  [FAILED] should provision the cluster",
				"  Unexpected error:",
				"      <*errors.errorString | 0x165419b3e300>: ",
				`      cluster e2e-nico-forge-20261107-030929 has not progressed for 15m0s of its 45m0s: provisioning still reports "Waiting for a control plane node to be ready to join" and no node has reached a new milestone`,
				"      ",
				"  occurred",
				"  In [It] at: /work/controller/test/e2e/tenant_test.go:564",
				exit1,
			),
			wantRule:   "tenant-control-plane-never-joined",
			wantClass:  "tenant-control-plane-join",
			wantAction: ActionRerunFailed,
			wantInEvid: "has not progressed for 15m0s",
		},
		{
			name: "jump host unreachable",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] Unexpected error:",
				"      <*errors.errorString | 0x1>: ",
				"      forge: 10.0.0.7 not reachable through jump.example after 20m0s: teleport: ssh to ubuntu@10.0.0.7 through jump.example: ssh: handshake failed: EOF",
				exit1,
			),
			wantRule:   "jump-host-unreachable",
			wantClass:  "forge-node-unreachable",
			wantAction: ActionRerunFailed,
			wantInEvid: "not reachable through",
		},
		{
			name: "node readiness timeout",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] in [It] - /work/controller/test/e2e/node_readiness_test.go:48 @ 11/13/26 04:05:56.073",
				"  [FAILED] A spec timeout occurred and then the following failure was recorded in the timedout node before it exited:",
				"  Timed out after 2700.002s.",
				"  [FAILED] in [It] - /work/controller/test/e2e/tpm_attestation_test.go:72 @ 11/13/26 04:06:05.347",
				exit1,
			),
			wantRule:   "node-readiness-timeout",
			wantClass:  "tenant-node-not-ready",
			wantAction: ActionRerunFailed,
			wantInEvid: "Timed out after 2700",
		},
		{
			name: "site cloud-init",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] Unexpected error:",
				"      <*errors.errorString | 0x1>: ",
				"      forge: cloud-init on nke-e2e-site-3d6e8c failed, and node preflight would reject it: ('scripts_user', RuntimeError('Runparts: 1 failures'))",
				exit1,
			),
			wantRule:   "site-cloud-init-failed",
			wantClass:  "site-cloud-init",
			wantAction: ActionNone,
			wantInEvid: "cloud-init on nke-e2e-site-3d6e8c failed",
		},
		{
			name: "cert-manager",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] Unexpected error:",
				"      <*errors.errorString | 0x1>: ",
				"      clusterctl init: Process exited with status 1 Installing cert-manager Waiting for cert-manager to be available... Error: context deadline exceeded",
				exit1,
			),
			wantRule:   "cert-manager-deadline",
			wantClass:  "site-cert-manager",
			wantAction: ActionNone,
			wantInEvid: "Waiting for cert-manager",
		},
		{
			name: "environment declaration",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] in [BeforeAll] - /work/controller/test/e2e/site_controller_test.go:173 @ 10/14/26 22:00:04.579",
				"  [FAILED] Unexpected error:",
				"      <*errors.errorString | 0x1>: ",
				`      forge: no jump host for site "ytl", and its machines are reachable only through one`,
				"  In [BeforeAll] at: /work/controller/test/e2e/site_controller_test.go:173",
				exit1,
			),
			wantRule:   "environment-declaration-incomplete",
			wantClass:  "environment-config",
			wantAction: ActionNone,
			wantInEvid: "no jump host for site",
		},
		{
			name: "ansi colour and timestamps are stripped",
			step: "Record the resolved inputs",
			log: "2026-11-16T10:00:00.0000000Z \x1b[31;1m##[error]the resolved-inputs table does not cover inputs.resolution; add a row\x1b[0m\n" +
				"2026-11-16T10:00:00.0000001Z " + exit1 + "\n",
			wantRule:   "resolved-inputs-row-missing",
			wantClass:  "resolved-inputs-row-missing",
			wantAction: ActionNone,
			wantInEvid: "inputs.resolution",
		},
		{
			name: "the first failing spec is the cause, later ones are fallout",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] should provision the cluster",
				"  Unexpected error:",
				"      <*errors.errorString | 0x1>: ",
				`      cluster c has not progressed for 15m0s of its 45m0s: provisioning still reports "Waiting for a control plane node to be ready to join" and no node has reached a new milestone`,
				"  occurred",
				"  [FAILED] Unexpected error:",
				"      <*errors.errorString | 0x2>: ",
				"      forge has no usable allocation for this run: cpu.arm (id): need 2, unusedUsable=1",
				exit1,
			),
			wantRule:   "tenant-control-plane-never-joined",
			wantClass:  "tenant-control-plane-join",
			wantAction: ActionRerunFailed,
			wantInEvid: "has not progressed",
		},
		{
			name: "workflow source in the log is not evidence",
			step: "Check every artifact the plan names",
			log: log(
				"##[group]Run set -Eeuo pipefail",
				`echo "::error::could not confirm os-image/releases/v1/amd64/x.qcow2 exists: HTTP 403"`,
				"cat > cfg.yaml <<EOF",
				"os-image/releases/v1/amd64/x.qcow2 exists: HTTP 403",
				"EOF",
				"shell: /usr/bin/bash -e {0}",
				"##[endgroup]",
				"##[error]something else entirely",
				exit1,
			),
			wantClass:  ClassUnknown,
			wantAction: ActionNone,
		},
		{
			name: "source that quotes a rule's phrase is not evidence",
			step: "Run the e2e suite",
			log: log(
				"##[group]Run ./hack/e2e.sh",
				"if ! grep -q 'Waiting for cert-manager to be available... Error:' out.log; then",
				"##[endgroup]",
				"##[error]something else entirely",
				exit1,
			),
			wantClass:  ClassUnknown,
			wantAction: ActionNone,
		},
		{
			name: "an unterminated heredoc in the source does not hide a real error",
			step: "Check every artifact the plan names",
			log: log(
				"##[group]Run ./check.sh",
				`echo "usage: cat <<EOF"`,
				"##[endgroup]",
				"##[error]os-image/releases/v1.2.3 exists: HTTP 403",
				exit1,
			),
			wantRule:   "os-image-not-published",
			wantClass:  "os-image-not-published",
			wantAction: ActionRerunImageBuild,
			wantInEvid: "HTTP 403",
		},
		{
			name: "what an earlier step printed is not the failure of the next",
			step: "Free disk space",
			log: log(
				"##[group]Run actions/download-artifact@v8",
				"##[endgroup]",
				"Artifact not found for name: qualify-site-inputs",
				"##[group]Run ./free-disk.sh",
				"##[endgroup]",
				"##[error]No space left on device",
				exit1,
			),
			wantClass:  ClassUnknown,
			wantAction: ActionNone,
		},
		{
			name: "cert-manager deadline with the error on its own line",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] should install the NVKE stack",
				"  Unexpected error:",
				"      <*fmt.wrapError | 0x25251a77a080>: ",
				"      clusterctl init: Process exited with status 1",
				"      Fetching providers",
				"      Installing cert-manager version=\"v1.21.1\"",
				"      Waiting for cert-manager to be available...",
				"      Error: context deadline exceeded",
				exit1,
			),
			wantRule:   "cert-manager-deadline",
			wantClass:  "site-cert-manager",
			wantAction: ActionNone,
			wantInEvid: "Error: context deadline exceeded",
		},
		{
			name: "cert-manager progress that did not fail is not the deadline",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] Unexpected error:",
				"      clusterctl init: Process exited with status 1 Installing cert-manager Waiting for cert-manager to be available... Installing provider infrastructure-nico Error: failed to apply",
				exit1,
			),
			wantClass:  ClassUnknown,
			wantAction: ActionNone,
		},
		{
			name: "a Ginkgo << marker is not source",
			step: "Run the e2e suite",
			log: log(
				"<< End Additional Progress Reports",
				"  [FAILED] Expected success, but got an error:",
				"      <*errors.errorString | 0x1>: ",
				"      forge has no usable allocation for this run: cpu.arm (id): need 2, unusedUsable=1",
				exit1,
			),
			wantRule:   "forge-no-usable-allocation",
			wantClass:  "forge-capacity",
			wantAction: ActionWaitThenRerun,
			wantInEvid: "need 2, unusedUsable=1",
		},
		{
			name: "a plain Fail message is the failure",
			step: "Run the e2e suite",
			log: log(
				"  [FAILED] in [BeforeAll] - /work/controller/test/e2e/site_controller_test.go:200 @ 10/27/26 02:45:29.311",
				"• [FAILED] [0.000 seconds]",
				"",
				`  [FAILED] -e2e.site-stage "deployment" is not provision, bootstrap or full`,
				"  In [BeforeAll] at: /work/controller/test/e2e/site_controller_test.go:200",
				exit1,
			),
			wantRule:   "environment-declaration-incomplete",
			wantClass:  "environment-config",
			wantAction: ActionNone,
			wantInEvid: "is not provision, bootstrap or full",
		},
		{
			name:       "no rule matches",
			step:       "Run the e2e suite",
			log:        log("  [FAILED] Unexpected error:", "      a new kind of failure", exit1),
			wantClass:  ClassUnknown,
			wantAction: ActionNone,
		},
	}

	covered := map[string]bool{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Extract(strings.NewReader(tt.log))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			got := cfg.Classify("", tt.step, e)

			if got.Class != tt.wantClass || got.Rule != tt.wantRule || got.Action != tt.wantAction {
				t.Fatalf("got class=%q rule=%q action=%q, want class=%q rule=%q action=%q\nevidence:\n%s",
					got.Class, got.Rule, got.Action, tt.wantClass, tt.wantRule, tt.wantAction, e.Text())
			}

			if !strings.Contains(got.Evidence, tt.wantInEvid) {
				t.Fatalf("evidence %q does not contain %q", got.Evidence, tt.wantInEvid)
			}

			if got.Note == "" {
				t.Fatal("a result must carry a note")
			}

			covered[got.Rule] = true
		})
	}

	// A shipped rule that no case here reaches is untested, and one that
	// shadows another is dead. Both fail this.
	for _, r := range cfg.Rules {
		if !covered[r.Name] {
			t.Errorf("rule %q matches none of the cases above", r.Name)
		}
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name  string
		rules string
		want  string
	}{
		{"no rules", "rules: []", "has no rules"},
		{"no name", "rules:\n  - {class: c, match: x, action: none, note: n}", "has no name"},
		{"duplicate name", "rules:\n  - {name: a, class: c, match: x, action: none, note: n}\n  - {name: a, class: c, match: x, action: none, note: n}", "used by an earlier rule"},
		{"no class", "rules:\n  - {name: a, match: x, action: none, note: n}", "has no class"},
		{"reserved class", "rules:\n  - {name: a, class: unknown, match: x, action: none, note: n}", "reserved"},
		{"unknown action", "rules:\n  - {name: a, class: c, match: x, action: delete-everything, note: n}", "is not one of"},
		{"no note", "rules:\n  - {name: a, class: c, match: x, action: none}", "has no note"},
		{"no match", "rules:\n  - {name: a, class: c, action: none, note: n}", "has no match"},
		{"bad match", "rules:\n  - {name: a, class: c, match: '(', action: none, note: n}", "match:"},
		{"bad step", "rules:\n  - {name: a, class: c, step: '(', match: x, action: none, note: n}", "step:"},
		{"unknown field", "rules:\n  - {name: a, class: c, match: x, action: none, note: n, retries: 3}", "unknown field"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.rules))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestStepNarrowsARule(t *testing.T) {
	c, err := Parse([]byte("rules:\n  - {name: a, class: c, step: '^Plan$', match: boom, action: none, note: n}"))
	if err != nil {
		t.Fatal(err)
	}

	e := Evidence{Annotations: []string{"boom"}}

	if got := c.Classify("", "Plan", e); got.Rule != "a" {
		t.Fatalf("on the Plan step got rule %q, want a", got.Rule)
	}

	if got := c.Classify("", "Run the e2e suite", e); got.Class != ClassUnknown {
		t.Fatalf("on another step got class %q, want unknown", got.Class)
	}
}

func TestAnOverlongLineDoesNotStopTheLog(t *testing.T) {
	cfg, err := Load(shipped)
	if err != nil {
		t.Fatalf("the shipped rules do not load: %v", err)
	}

	// A teardown dump on one line, longer than maxLine, then the real error.
	huge := strings.Repeat("x", 3*maxLine)
	in := log(huge, "##[error]os-image/releases/v1.2.3/amd64/x.qcow2 exists: HTTP 403", exit1)

	e, err := Extract(strings.NewReader(in))
	if err != nil {
		t.Fatalf("an overlong line failed the extraction: %v", err)
	}

	got := cfg.Classify("", "Check every artifact the plan names", e)
	if got.Rule != "os-image-not-published" {
		t.Errorf("rule = %q, want os-image-not-published; the line after the overlong one was lost", got.Rule)
	}
}

func TestAJobFilterKeepsARuleToItsJobs(t *testing.T) {
	c, err := Parse([]byte("rules:\n  - {name: a, class: c, job: '^forge-', match: boom, action: none, note: n}"))
	if err != nil {
		t.Fatal(err)
	}

	e := Evidence{Annotations: []string{"boom"}}

	if got := c.Classify("forge-gb200-amd64", "Run", e); got.Rule != "a" {
		t.Fatalf("a job the filter names: got %+v", got)
	}

	if got := c.Classify("hosted-amd64", "Run", e); got.Class != ClassUnknown {
		t.Fatalf("a job the filter does not name: got %+v", got)
	}

	if _, err := Parse([]byte("rules:\n  - {name: a, class: c, job: '(', match: boom, action: none, note: n}")); err == nil {
		t.Fatal("a bad job expression must be rejected")
	}
}

func TestRulesFilesAreJoinedInOrderAndNamesStayUnique(t *testing.T) {
	own := []byte("rules:\n  - {name: own, class: c1, match: boom, action: none, note: n}")
	shared := []byte("rules:\n  - {name: shared, class: c2, match: boom, action: none, note: n}")

	c, err := Parse(own, shared)
	if err != nil {
		t.Fatal(err)
	}

	e := Evidence{Annotations: []string{"boom"}}

	if got := c.Classify("", "", e); got.Rule != "own" {
		t.Fatalf("the first file's rule goes first: got %+v", got)
	}

	if c, _ = Parse(shared, own); c.Classify("", "", e).Rule != "shared" {
		t.Fatal("order follows the order given")
	}

	if _, err := Parse(own, own); err == nil {
		t.Fatal("a name used in two files must be rejected")
	}
}
