// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// credential-audit emits one versioned operator evidence observation to stdout.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("credential-audit", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // Flag errors can echo caller-supplied argument values.
	action := flags.String("action", "", "check, approve, renew, rotate or verify")
	credential := flags.String("credential", credentialaudit.TypeKubeadm, "credential type from the audit schema")
	plane := flags.String("plane", credentialaudit.PlaneT1, "t1 or t2")
	cluster := flags.String("cluster", "", "external cluster identity (hashed)")
	node := flags.String("node", "", "external node identity (hashed)")
	clusterID := flags.String("cluster-id", "", "managed cluster UUID (alternative to --cluster)")
	nodeID := flags.String("node-id", "", "managed node UUID (alternative to --node)")
	operation := flags.String("operation", "", "stable evidence run identifier (hashed)")
	actor := flags.String("actor", "", "self-reported operator subject (hashed)")
	outcome := flags.String("outcome", "", "success, failure or no_op")
	reason := flags.String("reason", "", "fixed audit reason code")
	phase := flags.String("phase", "", "optional fixed lifecycle phase")
	timestamp := flags.String("timestamp", "", "RFC3339 timestamp; default current UTC time")
	certificate := flags.String("certificate", "", "optional public certificate-only PEM file; maximum 1 MiB")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.PrintDefaults()
			return nil
		}
		return errors.New("invalid audit arguments; use --help")
	}
	if flags.NArg() != 0 || *operation == "" || *actor == "" {
		return errors.New("audit requires named arguments, --operation and --actor")
	}
	observedAt := time.Now().UTC()
	if *timestamp != "" {
		parsed, err := time.Parse(time.RFC3339Nano, *timestamp)
		if err != nil {
			return errors.New("invalid audit timestamp; RFC3339 required")
		}
		observedAt = parsed
	}
	target := credentialaudit.Target{ClusterID: *clusterID, NodeID: *nodeID}
	if *cluster != "" {
		target.ClusterHash = credentialaudit.Hash("cluster", *cluster)
	}
	if *node != "" {
		target.NodeHash = credentialaudit.Hash("node", *node)
	}
	event := credentialaudit.Event{
		Timestamp: observedAt, Action: *action, Credential: *credential, Plane: *plane,
		Target: target, Actor: credentialaudit.Actor{
			Kind:        credentialaudit.ActorOperatorReported,
			SubjectHash: credentialaudit.Hash("actor", *actor),
		},
		Outcome: *outcome, Reason: *reason, Phase: *phase,
		Correlation: credentialaudit.Correlation{OperationHash: credentialaudit.Hash("operation", *operation)},
	}
	if *certificate != "" {
		fingerprints, err := publicFingerprints(*certificate)
		if err != nil {
			return err
		}
		event.Fingerprints = fingerprints
	}
	validated, err := credentialaudit.New(event)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(validated); err != nil {
		return errors.New("could not write audit evidence")
	}
	return nil
}

func publicFingerprints(path string) ([]string, error) {
	file, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maximumBytes = 1024 * 1024
	contents, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil || len(contents) > maximumBytes {
		return nil, errors.New("could not read bounded public certificate input")
	}
	return credentialaudit.CertificateFingerprints(contents)
}
