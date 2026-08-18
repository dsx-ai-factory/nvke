// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build tools
// +build tools

package main

import (
	"fmt"
	"os"
	"strings"
)

const serverGen = "server.gen.go"

// The exact string we expect from oapi-codegen (with literal tabs)
const find = `
	var body DeleteNodeJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
		return
	}
	request.Body = &body
`

// The exact replacement (with literal tabs)
const replace = `
	if r.ContentLength > 0 {
		var body DeleteNodeJSONRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
			return
		}
		request.Body = &body
	}
`

// Patch generated server code to handle optional request bodies correctly.
// This is a workaround for oapi-codegen strict-server not respecting required: false.
// for optional request bodies.
//
// This program does a simple exact string replacement and will FAIL LOUDLY if the
// generated code structure changes. This is intentional to catch changes in the
// generated code that are not intended. If the generated code structure changes,
// the 2 constants above likely need to be updated.
func main() {
	content, err := os.ReadFile(serverGen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", serverGen, err)
		os.Exit(1)
	}

	// Check if already patched
	if strings.Contains(string(content), replace) {
		fmt.Println("✓ Already patched, skipping")
		return
	}

	// Check that the original pattern exists exactly once
	count := strings.Count(string(content), find)
	if count != 1 {
		fmt.Fprintf(os.Stderr, "Error: Expected pattern found %d times in %s (expected exactly 1)\n", count, serverGen)
		fmt.Fprintln(os.Stderr, "The generated code structure may have changed. Please update this script, or remove if no longer necessary.")
		os.Exit(1)
	}

	// Do the replacement
	newContent := strings.Replace(string(content), find, replace, 1)

	if err := os.WriteFile(serverGen, []byte(newContent), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", serverGen, err)
		os.Exit(1)
	}

	fmt.Println("✓ Successfully patched DeleteNode to handle optional request body")
}
