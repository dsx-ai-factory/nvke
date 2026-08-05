// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

//go:build tools
// +build tools

package main

import (
	"fmt"
	"os"
	"strings"
)

// generatedFiles are the oapi-codegen outputs that need a license header. They
// carry a "Code generated ... DO NOT EDIT." marker, so addlicense skips them and
// never adds the header itself; this post-generation step supplies it instead.
var generatedFiles = []string{"models.gen.go", "server.gen.go", "client.gen.go"}

// licenseMarker is the first line of the canonical header, used for the
// idempotency check.
const licenseMarker = "// SPDX-FileCopyrightText"

// licenseHeader is the repo's canonical 9-line SPDX/NVIDIA header block, copied
// from generate.go. Keep it byte-for-byte identical to the boilerplate so the
// file-license-check (addlicense) job stays satisfied.
const licenseHeader = `// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.`

// Prepend the canonical NVIDIA license header to the generated API files.
//
// The header is inserted AFTER any leading build-constraint lines (//go:build /
// // +build) and their required trailing blank line, so for server.gen.go the
// "//go:build go1.22" constraint remains the first line of the file. The
// "Code generated ... DO NOT EDIT." marker stays above the package clause so Go
// tooling still treats the files as generated. This program is idempotent: if a
// file already carries the header at the insertion point it is left untouched.
func main() {
	for _, name := range generatedFiles {
		if err := prependLicense(name); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing %s: %v\n", name, err)
			os.Exit(1)
		}
	}
}

func prependLicense(name string) error {
	content, err := os.ReadFile(name)
	if err != nil {
		return err
	}

	lines := strings.Split(string(content), "\n")

	// Build constraints must remain at the very top of the file, so find the
	// insertion point just past any leading //go:build / // +build lines.
	insertAt := 0
	for insertAt < len(lines) &&
		(strings.HasPrefix(lines[insertAt], "//go:build") || strings.HasPrefix(lines[insertAt], "// +build")) {
		insertAt++
	}
	// Preserve the blank line that separates the build constraint from the body.
	if insertAt > 0 && insertAt < len(lines) && lines[insertAt] == "" {
		insertAt++
	}

	// Idempotent: header already present at the insertion point, nothing to do.
	if insertAt < len(lines) && strings.HasPrefix(lines[insertAt], licenseMarker) {
		fmt.Printf("✓ %s already has license header, skipping\n", name)
		return nil
	}

	header := strings.Split(licenseHeader, "\n")

	newLines := make([]string, 0, len(lines)+len(header)+1)
	newLines = append(newLines, lines[:insertAt]...)
	newLines = append(newLines, header...)
	newLines = append(newLines, "") // blank line separating header from the rest
	newLines = append(newLines, lines[insertAt:]...)

	if err := os.WriteFile(name, []byte(strings.Join(newLines, "\n")), 0o644); err != nil {
		return err
	}

	fmt.Printf("✓ Prepended license header to %s\n", name)
	return nil
}
