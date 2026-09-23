// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"strings"
	"testing"
)

// TestDeleteCmdRequiresName verifies that delete cluster requires exactly one arg.
func TestDeleteCmdRequiresName(t *testing.T) {
	cmd := NewDeleteCmd(clientFnErr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when cluster name is omitted")
	}
}

// TestDeleteCmdReachesClient verifies that a name arg routes to the client call.
func TestDeleteCmdReachesClient(t *testing.T) {
	cmd := NewDeleteCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster"})
	if err := cmd.Execute(); !isNotConfigured(err) {
		t.Errorf("expected no-site error, got: %v", err)
	}
}

func TestDeleteClusterSendsDeleteForTheResolvedID(t *testing.T) {
	seed := seedCluster("doomed")
	fake, clientFn := newFakeCWE(t, seed)

	out, err := run(t, NewDeleteCmd(clientFn), "doomed")
	if err != nil {
		t.Fatalf("delete cluster doomed: %v", err)
	}

	want := []string{
		"GET /api/clusters?name=doomed",
		"DELETE /api/clusters/" + seed.ID.String(),
	}
	seen := fake.seen()
	if len(seen) != len(want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}
	if left := fake.stored(); len(left) != 0 {
		t.Errorf("fake still holds %d clusters, want none", len(left))
	}
	if !strings.Contains(out, `"doomed" deletion started`) {
		t.Errorf("output does not confirm deletion:\n%s", out)
	}
}
