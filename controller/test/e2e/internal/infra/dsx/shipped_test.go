// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"path/filepath"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/environment"
)

// The checked-in environments/dsx.yaml, read the way a run reads it.
//
// Both failures this guards were in the file rather than in Go, so no Go test
// could have caught them, and both cost a bare-metal run to find:
//
//   - nodeUser defaulted to dgxc-admin, the account the manual procedure's
//     internal image bakes. The lane boots the OSS os-image, which bakes
//     ubuntu, so cloud-init wrote authorized_keys for an account that did not
//     exist and the dial was refused for its whole 35-minute budget.
//   - machines pinned one host, so a leftover instance on it failed every run
//     with 409 while the rest of the datahall sat free.
//
// Unset environment variables expand to empty, which is what a developer
// checkout has, so this asserts the shape of the file rather than any
// identifier in it.
func TestShippedEnvironmentLeavesPlacementAndAccountToTheDefaults(t *testing.T) {
	env, err := environment.LoadPath("dsx", filepath.Join("..", "..", "..", "environments", "dsx.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped environments/dsx.yaml: %v", err)
	}
	decoded, err := Decode(env.Infrastructure)
	if err != nil {
		t.Fatalf("decoding it as the dsx dialect: %v", err)
	}
	if len(decoded.Sites) == 0 {
		t.Fatal("the shipped file declares no sites")
	}

	for key, site := range decoded.Sites {
		if site.NodeUser != "" {
			t.Errorf("site %q pins nodeUser=%q; leave it empty so it follows the image the run boots "+
				"(the OSS os-image bakes %q)", key, site.NodeUser, "ubuntu")
		}
		if len(site.Machines) != 0 {
			t.Errorf("site %q pins %d machine(s); leave machines out so the datahall picks one that is "+
				"free and healthy", key, len(site.Machines))
		}
		if len(site.InstanceTypes) == 0 {
			t.Errorf("site %q declares no instanceTypes; they place the tenant nodes and, with no machine "+
				"pinned, the site controller too", key)
		}
	}
}
