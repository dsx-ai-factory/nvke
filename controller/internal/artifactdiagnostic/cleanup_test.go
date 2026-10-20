// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedCleanupAfterCancellation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cleanup failure"}[fail], func(t *testing.T) {
			output := t.TempDir()
			tools := t.TempDir()
			for _, file := range []string{"owned-cluster", "kubeconfig"} {
				if err := os.WriteFile(filepath.Join(output, file), []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, tool := range []string{"kubectl", "docker", "kind"} {
				body := "#!/bin/sh\nexit 0\n"
				if fail && tool == "kind" {
					body = "#!/bin/sh\nexit 9\n"
				}
				if err := os.WriteFile(filepath.Join(tools, tool), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			log, err := os.Create(filepath.Join(output, "execution.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			r := runner{options: Options{Output: output, Cluster: "iv2-fixture"}, log: log}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err = r.cleanupController(ctx)
			if (err != nil) != fail {
				t.Fatalf("cleanup outcome: %v", err)
			}
			if fail {
				if r.report.Cleanup != cleanupFailed || r.report.CleanupFailure == "" {
					t.Fatal("cleanup failure hidden")
				}
				if _, err := os.Stat(filepath.Join(output, "owned-cluster")); err != nil {
					t.Fatal("recovery owner removed")
				}
			} else {
				if r.report.Cleanup != complete {
					t.Fatal("cleanup incomplete")
				}
				for _, file := range []string{"owned-cluster", "kubeconfig"} {
					if _, err := os.Stat(filepath.Join(output, file)); !os.IsNotExist(err) {
						t.Fatalf("cleanup left %s", file)
					}
				}
			}
		})
	}
}
