// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real files and SHA256 establish the local handoff checks. Only qemu-img is
// substituted; these contracts do not claim guest boot or QCOW2 acceptance.
func TestUnit_LocalOSImageRequiresExactPinnedRegularFile(t *testing.T) {
	library, err := filepath.Abs("../../hack/ci/nico-vms-os-image.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"exact", "missing hash", "malformed hash", "wrong hash", "changed bytes", "missing file", "relative path", "symlink", "empty file", "directory", "whitespace", "backslash", "variable expansion", "tag collision", "orphan hash", "qemu check failed"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			image := filepath.Join(root, "node.qcow2")
			payload := []byte("local disk transport fixture; qemu-img owns QCOW2 validation")
			if err := os.WriteFile(image, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			pin, tag, checkExit := fmt.Sprintf("%x", sha256.Sum256(payload)), "", "0"
			switch scenario {
			case "missing hash":
				pin = ""
			case "malformed hash":
				pin = strings.Repeat("g", 64)
			case "wrong hash":
				pin = strings.Repeat("a", 64)
			case "changed bytes":
				if err := os.WriteFile(image, []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing file":
				image = filepath.Join(root, "missing.qcow2")
			case "relative path":
				image = "node.qcow2"
			case "symlink":
				link := filepath.Join(root, "linked.qcow2")
				if err := os.Symlink(image, link); err != nil {
					t.Fatal(err)
				}
				image = link
			case "empty file":
				if err := os.WriteFile(image, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				image = root
			case "whitespace", "backslash", "variable expansion":
				image = filepath.Join(root, map[string]string{"whitespace": "disk name.qcow2", "backslash": `disk\name.qcow2`, "variable expansion": "${other}.qcow2"}[scenario])
				if err := os.WriteFile(image, payload, 0o600); err != nil {
					t.Fatal(err)
				}
			case "tag collision":
				tag = "must-not-pull"
			case "orphan hash":
				image = ""
			case "qemu check failed":
				checkExit = "3"
			}
			marker := filepath.Join(root, "qemu-check")
			qemu := "#!/bin/sh\n[ \"$#\" -eq 4 ] && [ \"$1\" = check ] && [ \"$2\" = -f ] && [ \"$3\" = qcow2 ] && [ \"$4\" = \"$NKE_E2E_OS_IMAGE_FILE\" ] || exit 91\nprintf checked >\"$CHECK_MARKER\"\nexit \"$CHECK_EXIT\"\n"
			if err := os.WriteFile(filepath.Join(root, "qemu-img"), []byte(qemu), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; fail(){ printf '%s\n' "$*" >&2; exit 1; }; nico_vms_local_os_image`, "contract", library)
			cmd.Env = []string{
				"PATH=" + root + ":" + os.Getenv("PATH"), "NKE_E2E_OS_IMAGE_FILE=" + image,
				"NKE_E2E_OS_IMAGE_SHA256=" + pin, "NKE_E2E_OS_IMAGE_TAG=" + tag,
				"CHECK_MARKER=" + marker, "CHECK_EXIT=" + checkExit,
			}
			out, err := cmd.CombinedOutput()
			if scenario == "exact" {
				if err != nil || strings.TrimSpace(string(out)) != image {
					t.Fatalf("local image selection: %s %v", out, err)
				}
			} else if err == nil {
				t.Fatalf("invalid local image accepted: %s", out)
			}
			_, checkErr := os.Stat(marker)
			if scenario == "exact" || scenario == "qemu check failed" {
				if checkErr != nil {
					t.Fatal("QCOW2 owner was not invoked", checkErr)
				}
			} else if !os.IsNotExist(checkErr) {
				t.Fatal("unsafe/unpinned input reached QCOW2 work", checkErr)
			}
		})
	}
}
