// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command gendocs writes the nvkectl Markdown reference.
//
// It is run by `make -C cli docs`, and verified by `make -C cli docs-check`.
// Neither target needs a credential, a kubeconfig, a configured site or a
// reachable cluster: this program builds the command tree and reads its
// metadata, and runs no nvkectl command.
//
// It lives under hack/ rather than cmd/ so that it is not one of the binaries
// a release builds. `go build ./...` still compiles it, and the linter still
// covers it.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dsx-ai-factory/nvke/cli/internal/clidocs"
	"github.com/dsx-ai-factory/nvke/cli/internal/nvkectl"
)

func main() {
	dir := flag.String("output", "docs/reference", "Directory to write the reference into")
	flag.Parse()

	// Before the tree is built, not after: two flag defaults are read from the
	// environment by the command constructors.
	clidocs.PinEnvironment()

	root := nvkectl.NewRootCmd(
		clidocs.PinnedVersion,
		clidocs.PinnedCommit,
		clidocs.PinnedTreeState,
		clidocs.PinnedBuildDate,
	)

	if err := clidocs.Generate(root, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Printf("  wrote the nvkectl reference to %s\n", *dir)
}
