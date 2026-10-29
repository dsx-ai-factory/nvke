// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture and the bootstrap cluster have to share one runtime: nvkectl
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

// systemctl is-active exits 0 when any listed unit is active, so readiness has
// to read each unit's state: spire-agent can still be queued behind the image
// import while containerd and swtpm-local are up.
func TestUnitsActive(t *testing.T) {
	units := []string{"containerd", "swtpm-local", "spire-agent"}
	tests := []struct {
		name    string
		out     string
		wantErr string
	}{
		{"all active", "active\nactive\nactive\n", ""},
		{"spire-agent still queued", "active\nactive\ninactive\n", "spire-agent=inactive"},
		{"two units down", "active\nactivating\nfailed\n", "swtpm-local=activating, spire-agent=failed"},
		{"too few lines", "active\n", "printed"},
		{"no output", "", "printed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := unitsActive(tt.out, units)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unitsActive = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("unitsActive = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
