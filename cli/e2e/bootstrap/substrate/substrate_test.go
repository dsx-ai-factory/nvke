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
	if ContainerCLI != "nerdctl" {
		t.Errorf("ContainerCLI = %q, want %q", ContainerCLI, "nerdctl")
	}
}

// Every call site must go through ContainerCLI rather than naming a runtime
// directly. A single reintroduced "docker" literal would work on a CI runner
// that still has Docker installed, so nothing else in this package would
// notice — the suite would silently depend on Docker again.
//
// Source text rather than behaviour: these call sites shell out to a real
// runtime, so there is no seam to assert against without one present.
func TestNoDockerInvocations(t *testing.T) {
	// The whole tree under this package, not just this directory: cmd/nodes
	// is where a `docker ps` for a developer convenience target lived
	// unnoticed while a directory-only scan passed.
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(body), "\n") {
			// Trailing-quote match: catches "docker" and "/usr/bin/docker" alike.
			// The command position specifically -- "Dockerfile" is a filename
			// this package legitimately reads.
			if strings.Contains(line, `docker"`) {
				t.Errorf("%s: invokes docker directly; use ContainerCLI:\n\t%s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the package tree: %v", err)
	}
}
