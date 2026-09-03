// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command nodes drives the nkectl bootstrap-site E2E fixture: the fake
// pre-provisioned nodes real dispatch will target once bootstrap site stops
// being a stub. Invoked by `make -C cli nodes-up/nodes-down/nodes-status`,
// not run directly.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/NVIDIA/nke/cli/e2e/bootstrap/substrate"
)

// attestTimeout bounds the wait for every host to attest. Credential activation
// is a handful of TPM operations against a software TPM, so this is generous:
// past it, the bootstrap did not give the agents a server they can get into.
const attestTimeout = 3 * time.Minute

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: nodes <up|down|status|inventory|attested>")
		os.Exit(2)
	}

	ctx := context.Background()
	logf := func(format string, args ...any) { slog.Info(fmt.Sprintf(format, args...)) }
	cfg := substrate.DefaultConfig()

	var err error
	switch os.Args[1] {
	case "up":
		_, err = substrate.Start(ctx, cfg, logf)
	case "down":
		err = down(ctx, cfg, logf)
	case "status":
		err = status(ctx)
	case "hosts":
		err = writeHostsCSV(ctx)
	case "attested":
		err = attested(ctx, logf)
	default:
		fmt.Fprintf(os.Stderr,
			"unknown subcommand %q; want up, down, status, hosts, or attested\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodes:", err)
		os.Exit(1)
	}
}

func down(ctx context.Context, cfg substrate.Config, logf substrate.Logf) error {
	cfg.Keep = false // "down" always tears down, regardless of NKECTL_E2E_KEEP.
	s, err := substrate.Attach(cfg, logf)
	if err != nil {
		return err
	}
	s.Stop(ctx)
	return nil
}

// writeHostsCSV writes the node CSV that "nkectl generate sitespec" takes, with
// each running fixture host's TPM EK thumbprint filled in, and prints the shell
// assignments run.sh needs to build the site spec from it.
//
// The thumbprints and the bootstrap CA cannot be committed: a TPM's endorsement
// key is created along with the container, and a private key does not belong in
// a repository. Both are produced here against whatever this run started, and
// then reach the command the way an operator's would — the thumbprints as a
// column of the CSV, the CA named with --set.
//
// Output is three KEY=value lines on stdout and nothing else, so run.sh can
// source them.
func writeHostsCSV(ctx context.Context) error {
	out := os.Getenv("NKECTL_E2E_HOSTS_CSV")
	baselinePath := os.Getenv("NKECTL_E2E_ATTESTATION_BASELINE")
	if out == "" || baselinePath == "" {
		return fmt.Errorf("NKECTL_E2E_HOSTS_CSV and NKECTL_E2E_ATTESTATION_BASELINE are required")
	}

	s, err := tpmSubstrate(ctx, func(string, ...any) {})
	if err != nil {
		return err
	}

	var csv strings.Builder
	csv.WriteString("name,ip,tpm_ek_thumbprint\n")
	for _, node := range s.Nodes() {
		fmt.Fprintf(&csv, "%s,%s,%s\n", node.Name, node.IP, node.EKPubhash)
	}
	if err := os.WriteFile(out, []byte(csv.String()), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}

	// The CA the node images were built against.
	cfg := substrate.DefaultConfig()
	caCert, caKey, err := substrate.EnsureBootstrapCA(cfg.CADir)
	if err != nil {
		return err
	}

	baseline, err := s.CaptureAttestationBaseline(ctx)
	if err != nil {
		return err
	}
	if err := substrate.WriteAttestationBaseline(baselinePath, baseline); err != nil {
		return err
	}

	fmt.Printf("NKECTL_E2E_HOSTS_CSV=%s\n", out)
	fmt.Printf("NKECTL_E2E_SPIRE_CA_CERT=%s\n", caCert)
	fmt.Printf("NKECTL_E2E_SPIRE_CA_KEY=%s\n", caKey)
	return nil
}

// attested waits for every host's spire-agent to be admitted by the SPIRE
// server `nkectl bootstrap site` deployed.
//
// This is the whole check. The agents have been running since the containers
// booted, pointed at the address the command puts the server on, so nothing
// here configures or starts anything — if the bootstrap wired SPIRE correctly
// they get in, and if it did not they never do.
func attested(ctx context.Context, logf substrate.Logf) error {
	baselinePath := os.Getenv("NKECTL_E2E_ATTESTATION_BASELINE")
	if baselinePath == "" {
		return fmt.Errorf("NKECTL_E2E_ATTESTATION_BASELINE is required")
	}
	baseline, err := substrate.LoadAttestationBaseline(baselinePath)
	if err != nil {
		return err
	}
	s, err := tpmSubstrate(ctx, logf)
	if err != nil {
		return err
	}
	if err := s.WaitAttested(ctx, baseline, attestTimeout); err != nil {
		s.DumpDiagnostics(ctx)
		return err
	}
	logf("nodes: all %d host(s) attested to the SPIRE server", len(s.Nodes()))
	return nil
}

// tpmSubstrate attaches to the running fixture and reads its EK fingerprints.
func tpmSubstrate(ctx context.Context, logf substrate.Logf) (*substrate.Substrate, error) {
	s, err := substrate.Attach(substrate.DefaultConfig(), logf)
	if err != nil {
		return nil, err
	}
	return s, s.ReadEKPubhashes(ctx)
}

func status(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=nkectl-e2e=true",
		"--format", "table {{.Names}}\t{{.Status}}\t{{.Networks}}")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
