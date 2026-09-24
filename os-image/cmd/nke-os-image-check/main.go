// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nke-os-image-check probes an nke-agent endpoint over SPIFFE mTLS.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/nke/os-image/internal/check"
)

// version is set at build time via -X main.version=.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("nke-os-image-check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	versionFlag := fs.Bool("version", false, "Print the version and exit")
	endpoint := fs.String("endpoint", "", "nke-agent host or host:port (default port 15005)")
	socket := fs.String("spiffe-socket", check.DefaultSPIFFESocket, "SPIFFE workload API address")
	trustDomain := fs.String("expect-trust-domain", "", "If set, require the local SVID to be in this trust domain")
	tlsJWT := fs.Bool("tls-jwt", false, "Expect an NKEAuth record, matching an agent run with --grpc-auth-mode=tls-jwt")
	timeout := fs.Duration("timeout", 30*time.Second, "Overall probe timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *versionFlag {
		fmt.Printf("nke-os-image-check version: %s\n", version)
		return 0
	}
	if strings.TrimSpace(*endpoint) == "" {
		fmt.Fprintln(os.Stderr, "nke-os-image-check: --endpoint is required")
		fs.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	result, err := check.Probe(ctx, check.Options{
		Endpoint:          *endpoint,
		SPIFFESocket:      *socket,
		ExpectTrustDomain: *trustDomain,
		ExpectAuthRecord:  *tlsJWT,
	})
	if err != nil {
		slog.Error("probe failed", "err", err)
		return 1
	}
	attrs := []any{
		"endpoint", result.Endpoint,
		"client_svid", result.ClientSVID,
		"agent_version", result.AgentVersion,
		"kubeadm_status", result.Kubeadm,
		"services", result.Services,
	}
	if result.AuthRecord {
		attrs = append(attrs, "auth_record", true, "unverified_auth_subject", result.AuthSubject)
	}
	slog.Info("nke-agent reachable", attrs...)
	return 0
}
