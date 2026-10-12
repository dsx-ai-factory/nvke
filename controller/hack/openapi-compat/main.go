// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command openapi-compat compares api/openapi.yaml against a published baseline
// and fails when the change is backward incompatible.
//
// `make -C controller generate-check` already proves the committed Go matches
// the specification. It says nothing about whether the specification change
// itself is safe: a removed field, a narrowed type or a newly required property
// regenerates perfectly and breaks every existing client. This is the layer
// above that.
//
// The baseline is a published controller/api version tag, not the merge base.
// The tag is what a consumer can actually pin (`go get
// github.com/dsx-ai-factory/nvke/controller/api@v0.0.17`), so it is the only ref
// against which "backward compatible" means anything to them.
//
// Everything that is not provably compatible fails. A missing baseline, an
// unparsable specification, an unreadable report and an unrecognized report
// shape are all errors, never silent passes -- a compatibility gate that cannot
// run has not found the change compatible.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "openapi-compat: %v\n", err)
		os.Exit(1)
	}
}
