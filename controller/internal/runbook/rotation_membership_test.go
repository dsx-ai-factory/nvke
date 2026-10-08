// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRotationMembershipProbeMatchesExactVotersAndHealth(t *testing.T) {
	for _, test := range []struct {
		name, members string
		health        bool
		ok            bool
	}{
		{"converged", `{"members":[{"name":"cp-1"},{"name":"cp-0"}]}`, true, true},
		{"extra_member", `{"members":[{"name":"cp-0"},{"name":"cp-1"},{"name":"cp-2"}]}`, true, false},
		{"same_count_replacement", `{"members":[{"name":"cp-0"},{"name":"cp-2"}]}`, true, false},
		{"learner", `{"members":[{"name":"cp-0"},{"name":"cp-1","isLearner":true}]}`, true, false},
		{"unhealthy", `{"members":[{"name":"cp-0"},{"name":"cp-1"}]}`, false, false},
		{"malformed", `{"members":null}`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			script, err := Script("", "etcd-rotation-membership")
			require.NoError(t, err)
			path := filepath.Join(dir, "probe.sh")
			require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
			fake := `#!/usr/bin/env bash
case "$*" in
 "ps --label io.kubernetes.container.name=etcd -q") echo container;;
 "inspect container") echo '{"status":{"labels":{"io.kubernetes.pod.namespace":"kube-system","io.kubernetes.pod.name":"etcd-cp-0"}}}';;
 *"member list --write-out=json") printf '%s\n' "$TEST_MEMBERS";;
 *"endpoint health --cluster") [[ "$TEST_HEALTH" == true ]];;
 *) exit 2;;
esac
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "crictl"), []byte(fake), 0o700))
			command := exec.CommandContext(t.Context(), "bash", path, "cp-0", "cp-1")
			health := "false"
			if test.health {
				health = "true"
			}
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_MEMBERS="+test.members, "TEST_HEALTH="+health)
			output, err := command.CombinedOutput()
			if test.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Empty(t, output)
		})
	}
}
