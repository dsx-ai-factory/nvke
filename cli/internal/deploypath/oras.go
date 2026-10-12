// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ORASPuller pulls with the oras binary, as the install scripts do.
type ORASPuller struct {
	// Binary is the oras executable; "oras" when empty.
	Binary string
	// PlainHTTP lists registry hosts (host or host:port) served over plain
	// HTTP. Nothing is probed: a registry is used in the clear only when
	// listed.
	PlainHTTP []string
}

func (p ORASPuller) binary() string {
	if p.Binary == "" {
		return "oras"
	}
	return p.Binary
}

func (p ORASPuller) flags(ref string) []string {
	host := ref[:strings.IndexAny(ref+"/", "/")]
	for _, listed := range p.PlainHTTP {
		if listed == host {
			return []string{"--plain-http"}
		}
	}
	return nil
}

// Manifest fetches the manifest ref names.
func (p ORASPuller) Manifest(ref string) ([]byte, error) {
	args := append([]string{"manifest", "fetch"}, p.flags(ref)...)
	args = append(args, ref)
	cmd := exec.CommandContext(context.Background(), p.binary(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", p.binary(), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Blob fetches repository@digest into dest.
func (p ORASPuller) Blob(repository, digest, dest string) error {
	ref := repository + "@" + digest
	args := append([]string{"blob", "fetch"}, p.flags(ref)...)
	args = append(args, "--output", dest, ref)
	cmd := exec.CommandContext(context.Background(), p.binary(), args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", p.binary(), strings.Join(args, " "), err)
	}
	return nil
}
