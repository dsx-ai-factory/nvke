// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture and the bootstrap cluster have to share one runtime: nkectl
// loads its images from containerd, so a node container started through some
// other daemon would be on a network the cluster cannot reach, and images
// built through it would be invisible to the loader.
func TestContainerCLIIsNerdctl(t *testing.T) {
	if containerCLI != "nerdctl" {
		t.Errorf("containerCLI = %q, want %q", containerCLI, "nerdctl")
	}
}

// Every call site must go through containerCLI rather than naming a runtime
// directly. A single reintroduced "docker" literal would work on a CI runner
// that still has Docker installed, so nothing else in this package would
// notice — the suite would silently depend on Docker again.
//
// Source text rather than behaviour: these call sites shell out to a real
// runtime, so there is no seam to assert against without one present.
func TestNoDockerInvocations(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		// Test files are excluded: this one names the literal it is looking
		// for, and would report itself.
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		// The command position specifically. "Dockerfile" is a filename this
		// package legitimately reads, and the word appears in comments
		// explaining why Docker is not used.
		for _, line := range strings.Split(string(body), "\n") {
			// Trailing-quote match: catches "docker" and "/usr/bin/docker" alike.
			if strings.Contains(line, `docker"`) {
				t.Errorf("%s: invokes docker directly; use containerCLI:\n\t%s",
					name, strings.TrimSpace(line))
			}
		}
	}
}
