// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"
)

// A run that qualifies versions has to name all of them, and a missing one is
// reported together with the others so one run shows everything to fix.
func TestPickVersions(t *testing.T) {
	supplied := map[string]string{"kube": "1.36.3", "cni-ovn": "1.4.0", "controller": "v0.1.1"}

	t.Run("every one named", func(t *testing.T) {
		got, err := pickVersions(supplied, []string{"kube", "cni-ovn"})
		if err != nil {
			t.Fatal(err)
		}
		if got["kube"] != "1.36.3" || got["cni-ovn"] != "1.4.0" || len(got) != 2 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("a missing one is named with what the run did name", func(t *testing.T) {
		_, err := pickVersions(supplied, []string{"kube", "kind"})
		if err == nil {
			t.Fatal("expected an error, got none")
		}
		want := "-e2e.versions does not name kind; it names cni-ovn, controller, kube"
		if err.Error() != want {
			t.Errorf("error = %q, want %q", err, want)
		}
	})

	t.Run("every missing one is named at once", func(t *testing.T) {
		_, err := pickVersions(supplied, []string{"kind", "clusterctl", "nerdctl"})
		if err == nil || !strings.Contains(err.Error(), "does not name kind, clusterctl, nerdctl;") {
			t.Errorf("err = %v, want it to name all three", err)
		}
	})

	t.Run("a run that named nothing says so", func(t *testing.T) {
		_, err := pickVersions(map[string]string{}, []string{"kube", "cni-ovn"})
		if err == nil || !strings.Contains(err.Error(), "names no components, and kube, cni-ovn required") {
			t.Errorf("err = %v, want it to say nothing was named", err)
		}
	})
}
