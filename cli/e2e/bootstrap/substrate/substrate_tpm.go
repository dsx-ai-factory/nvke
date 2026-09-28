// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultSpireServerAddress is the address the node images' spire-agents
	// are built to look for, and therefore the address `nkectl bootstrap site`
	// must put its SPIRE server on. It is a fixed address on the substrate
	// network that the bootstrap cluster's node container is attached to —
	// standing in for the VIP a real site controller has.
	DefaultSpireServerAddress = "172.26.0.2"

	// nodeTrustBundlePath is where the bootstrap CA lands in the node image.
	nodeTrustBundlePath = "/etc/spire-agent/bootstrap-ca.crt"

	// DefaultSpireServerPort is the node port cwedeploy's external-service
	// manifest publishes SPIRE on
	// (internal/bootstrap/cwedeploy/manifests/spire-server-nodeport.yaml). It is fixed
	// rather than allocated because the agents are configured with it before
	// the cluster exists, so the two have to agree — and this is the other
	// half of that pair.
	DefaultSpireServerPort = "30081"

	attestationBaselineVersion = 1
)

// AttestationBaseline records where each node's journal stood before bootstrap
// started.
type AttestationBaseline struct {
	Version int                       `json:"version"`
	Nodes   []AttestationNodeBaseline `json:"nodes"`
}

// AttestationNodeBaseline describes one node's expected attestation result.
type AttestationNodeBaseline struct {
	Name          string `json:"name"`
	EKPubhash     string `json:"ek_pubhash"`
	JournalCursor string `json:"journal_cursor"`
}

// ReadEKPubhashes reads and records each host's TPM endorsement-key
// fingerprint. It returns an error if two hosts report the same fingerprint.
func (s *Substrate) ReadEKPubhashes(ctx context.Context) error {
	for i := range s.nodes {
		node := &s.nodes[i]
		thumbprint, err := s.readEKThumbprint(ctx, *node)
		if err != nil {
			return err
		}
		node.EKPubhash = thumbprint
	}

	// Two hosts sharing a fingerprint would be admitted under one SPIFFE ID and
	// make the check below vacuous. It is a real risk, not a theoretical one:
	// /sys/class/tpm is shared across containers, so a mistake in the vTPM
	// setup collapses every host onto whichever swtpm came up first.
	seen := map[string]string{}
	for _, node := range s.nodes {
		if other, duplicate := seen[node.EKPubhash]; duplicate {
			return fmt.Errorf("hosts %s and %s report the same EK fingerprint: their vTPMs are not isolated",
				other, node.Name)
		}
		seen[node.EKPubhash] = node.Name
	}
	return nil
}

// readEKThumbprint reads one host's EK, retrying while the TPM is busy.
//
// spire-agent has been running since the host booted and takes the TPM for the
// length of each attestation attempt, so a read that lands inside one gets
// "device or resource busy". That is contention, not a fault: the agent backs
// off between attempts and the device frees up. Retrying is what makes this
// read independent of what the agent happens to be doing, rather than a race
// that passes most of the time.
func (s *Substrate) readEKThumbprint(ctx context.Context, node Node) (string, error) {
	deadline := time.Now().Add(s.Config.ReadyTimeout)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := s.exec(ctx, node, "/usr/local/bin/get_tpm_pubhash")
		last = strings.TrimSpace(out)
		lastErr = err
		switch {
		case err == nil && len(last) == 64:
			return last, nil
		case err == nil:
			return "", fmt.Errorf("get_tpm_pubhash on %s printed %q, want a 64-character SHA-256",
				node.Name, last)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("could not read %s's EK thumbprint within %s; last output: %s: %w",
			node.Name, s.Config.ReadyTimeout, last, lastErr)
	}
	return "", fmt.Errorf("could not read %s's EK thumbprint within %s; last output: %s",
		node.Name, s.Config.ReadyTimeout, last)
}

// EKPubhashes returns every host's fingerprint, in inventory order.
func (s *Substrate) EKPubhashes() []string {
	hashes := make([]string, 0, len(s.nodes))
	for _, node := range s.nodes {
		hashes = append(hashes, node.EKPubhash)
	}
	return hashes
}

// CaptureAttestationBaseline records a journal cursor for every fixture node.
func (s *Substrate) CaptureAttestationBaseline(ctx context.Context) (AttestationBaseline, error) {
	baseline := AttestationBaseline{Version: attestationBaselineVersion}
	for _, node := range s.nodes {
		out, err := s.exec(ctx, node,
			"journalctl", "-u", "spire-agent", "--no-pager", "-n", "1", "--show-cursor")
		if err != nil {
			return AttestationBaseline{}, fmt.Errorf("reading %s's SPIRE journal cursor: %w", node.Name, err)
		}
		cursor, err := parseJournalCursor(out)
		if err != nil {
			return AttestationBaseline{}, fmt.Errorf("reading %s's SPIRE journal cursor: %w", node.Name, err)
		}
		baseline.Nodes = append(baseline.Nodes, AttestationNodeBaseline{
			Name:          node.Name,
			EKPubhash:     node.EKPubhash,
			JournalCursor: cursor,
		})
	}
	return baseline, nil
}

// WriteAttestationBaseline writes a baseline for the later attestation check.
func WriteAttestationBaseline(path string, baseline AttestationBaseline) error {
	body, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the attestation baseline: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating the attestation baseline directory: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing the attestation baseline: %w", err)
	}
	return nil
}

// LoadAttestationBaseline loads and validates a saved attestation baseline.
func LoadAttestationBaseline(path string) (AttestationBaseline, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return AttestationBaseline{}, fmt.Errorf("reading the attestation baseline: %w", err)
	}
	var baseline AttestationBaseline
	if err := json.Unmarshal(body, &baseline); err != nil {
		return AttestationBaseline{}, fmt.Errorf("decoding the attestation baseline: %w", err)
	}
	if baseline.Version != attestationBaselineVersion {
		return AttestationBaseline{}, fmt.Errorf("attestation baseline version is %d, want %d",
			baseline.Version, attestationBaselineVersion)
	}
	if len(baseline.Nodes) == 0 {
		return AttestationBaseline{}, fmt.Errorf("attestation baseline contains no nodes")
	}
	return baseline, nil
}

// WaitAttested waits until every host attests to the SPIRE server or timeout
// expires.
func (s *Substrate) WaitAttested(
	ctx context.Context,
	baseline AttestationBaseline,
	timeout time.Duration,
) error {
	if err := s.validateAttestationBaseline(baseline); err != nil {
		return err
	}
	// An active spire-agent can still fail attestation. Use journal results
	// written after the baseline instead of the systemd unit state.
	deadline := time.Now().Add(timeout)
	for _, expected := range baseline.Nodes {
		node := s.nodeByName(expected.Name)
		attested := false
		for !attested && time.Now().Before(deadline) {
			journal, err := s.exec(ctx, node,
				"journalctl", "-u", "spire-agent", "--no-pager", "--after-cursor", expected.JournalCursor)
			if err != nil {
				return fmt.Errorf("reading %s's SPIRE journal after the attestation baseline: %w\n%s",
					node.Name, err, journal)
			}
			refusal := lastLineContaining(journal, "nodeattestor(tpm)")
			switch {
			case strings.Contains(journal, "Node attestation was successful"):
				attested = true
			// A verdict from the server's TPM attestor is terminal: it is a
			// function of this host's EK and the allowlist, so retrying cannot
			// change it. Reporting it names the fault instead of timing out.
			case isTPMServerRefusal(refusal):
				return fmt.Errorf("the SPIRE server refused %s (EK %s):\n%s",
					node.Name, node.EKPubhash, refusal)
			default:
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
				}
			}
		}
		if !attested {
			journal, err := s.exec(ctx, node,
				"journalctl", "-u", "spire-agent", "--no-pager", "--after-cursor", expected.JournalCursor)
			if err != nil {
				return fmt.Errorf("reading %s's SPIRE journal after the attestation timeout: %w\n%s",
					node.Name, err, journal)
			}
			return fmt.Errorf("%s did not attest within %s\n%s", node.Name, timeout, journal)
		}
		s.log("substrate: %s attested as spiffe://%s/spire/agent/tpm/%s",
			node.Name, s.Config.SpireTrustDomain, node.EKPubhash)
	}
	return nil
}

func (s *Substrate) validateAttestationBaseline(baseline AttestationBaseline) error {
	if baseline.Version != attestationBaselineVersion {
		return fmt.Errorf("attestation baseline version is %d, want %d",
			baseline.Version, attestationBaselineVersion)
	}
	if len(baseline.Nodes) != len(s.nodes) {
		return fmt.Errorf("attestation baseline has %d nodes, but %d are running",
			len(baseline.Nodes), len(s.nodes))
	}
	seen := make(map[string]bool, len(baseline.Nodes))
	for _, expected := range baseline.Nodes {
		if seen[expected.Name] {
			return fmt.Errorf("attestation baseline contains node %q more than once", expected.Name)
		}
		seen[expected.Name] = true
		node := s.nodeByName(expected.Name)
		if node.Name == "" {
			return fmt.Errorf("attestation baseline names node %q, which is not running", expected.Name)
		}
		if expected.EKPubhash != node.EKPubhash {
			return fmt.Errorf("%s's EK changed after the attestation baseline was recorded: got %s, want %s",
				node.Name, node.EKPubhash, expected.EKPubhash)
		}
		if expected.JournalCursor == "" {
			return fmt.Errorf("attestation baseline has no journal cursor for %s", node.Name)
		}
	}
	return nil
}

func (s *Substrate) nodeByName(name string) Node {
	for _, node := range s.nodes {
		if node.Name == name {
			return node
		}
	}
	return Node{}
}

func parseJournalCursor(out string) (string, error) {
	const marker = "-- cursor: "
	for _, line := range strings.Split(out, "\n") {
		if cursor := strings.TrimSpace(strings.TrimPrefix(line, marker)); strings.HasPrefix(line, marker) && cursor != "" {
			return cursor, nil
		}
	}
	return "", fmt.Errorf("journalctl returned no cursor: %q", strings.TrimSpace(out))
}

func (s *Substrate) exec(ctx context.Context, node Node, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, ContainerCLI,
		append([]string{"exec", node.Container}, args...)...).CombinedOutput()
	return string(out), err
}

// isTPMServerRefusal reports whether a nodeattestor(tpm) journal line is the
// server rejecting this host, rather than the agent failing to build its
// attestation data and retrying.
//
// The distinction matters because only the first is terminal. The agent and
// get_tpm_pubhash contend for the TPM (see readEKThumbprint), so an attestation
// attempt that lands inside an EK read gets "device or resource busy" before it
// has spoken to the server at all, and the attempt after the agent's backoff
// succeeds. Both messages carry the nodeattestor(tpm) prefix, so matching that
// substring alone cannot tell a refusal from a retry -- and calling the retry
// terminal fails this check on a race it was already waiting out.
func isTPMServerRefusal(line string) bool {
	if line == "" {
		return false
	}
	// The agent says so itself when the failure is local: it never got as far
	// as an answer from the server.
	for _, local := range []string{
		"failed to generate attestation data",
		"device or resource busy",
	} {
		if strings.Contains(line, local) {
			return false
		}
	}
	return true
}

func lastLineContaining(text, needle string) string {
	var found string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			found = line
		}
	}
	return found
}

// defaultCADir keeps the bootstrap CA beside the module rather than in a user
// cache, because run.sh reads the same files to hand to `nkectl bootstrap
// site` and needs a path it can name without guessing at OS conventions.
func defaultCADir() string {
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Join(os.TempDir(), "nkectl-e2e-spire-ca")
	}
	return filepath.Join(wd, ".local", "spire-ca")
}

// defaultCacheDir holds downloaded build inputs between runs.
func defaultCacheDir() string {
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "nkectl-e2e-bootstrap")
	}
	return filepath.Join(os.TempDir(), "nkectl-e2e-bootstrap")
}
