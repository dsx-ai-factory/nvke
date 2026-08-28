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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/cli/e2e/bootstrap/substrate"
	"github.com/NVIDIA/nke/cli/internal/inventory"
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
	case "inventory":
		err = renderInventory(ctx)
	case "attested":
		err = attested(ctx, logf)
	default:
		fmt.Fprintf(os.Stderr,
			"unknown subcommand %q; want up, down, status, inventory, or attested\n", os.Args[1])
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

// renderInventory writes the base inventory back out with each node's TPM EK
// thumbprint filled in, and prints the path.
//
// The thumbprints have to reach `nkectl bootstrap site` the way an operator's
// would — as part of the node's inventory entry, which is where the ticket puts
// them and what the command reads. They cannot be committed to the fixture
// because a TPM's endorsement key is created with the container, so this fills
// them in against whichever hosts are running now.
//
// NKECTL_E2E_INVENTORY_IN and _OUT name the files; the path is printed on
// stdout and nothing else is, so run.sh can capture it.
func renderInventory(ctx context.Context) error {
	in := os.Getenv("NKECTL_E2E_INVENTORY_IN")
	out := os.Getenv("NKECTL_E2E_INVENTORY_OUT")
	baselinePath := os.Getenv("NKECTL_E2E_ATTESTATION_BASELINE")
	if in == "" || out == "" || baselinePath == "" {
		return fmt.Errorf("NKECTL_E2E_INVENTORY_IN, NKECTL_E2E_INVENTORY_OUT, and NKECTL_E2E_ATTESTATION_BASELINE are required")
	}

	s, err := tpmSubstrate(ctx, func(string, ...any) {})
	if err != nil {
		return err
	}
	thumbprints := map[string]string{}
	for _, node := range s.Nodes() {
		thumbprints[node.Name] = node.EKPubhash
	}

	inv, err := inventory.Load(in)
	if err != nil {
		return err
	}

	// The CA the node images were built against. It reaches the command the
	// same way an operator's would — named in the inventory — rather than
	// through a side channel the product does not have.
	cfg := substrate.DefaultConfig()
	caCert, caKey, err := substrate.EnsureBootstrapCA(cfg.CADir)
	if err != nil {
		return err
	}
	inv.Spire.CACertFile = caCert
	inv.Spire.CAKeyFile = caKey
	nodes := append([]*inventory.Node{&inv.Cluster.InitNode}, ptrs(inv.Cluster.JoinNodes)...)
	for _, node := range nodes {
		thumbprint, running := thumbprints[node.Name]
		if !running {
			return fmt.Errorf("inventory names node %q, which is not one of the running fixture hosts", node.Name)
		}
		node.TPMEKThumbprint = thumbprint
	}
	baseline, err := s.CaptureAttestationBaseline(ctx)
	if err != nil {
		return err
	}

	body, err := yaml.Marshal(inv)
	if err != nil {
		return fmt.Errorf("encoding the inventory: %w", err)
	}
	if err := os.WriteFile(out, body, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	if err := substrate.WriteAttestationBaseline(baselinePath, baseline); err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// ptrs lets the init node and the join nodes be walked as one list of things
// that can be written to.
func ptrs(nodes []inventory.Node) []*inventory.Node {
	out := make([]*inventory.Node, len(nodes))
	for i := range nodes {
		out[i] = &nodes[i]
	}
	return out
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
