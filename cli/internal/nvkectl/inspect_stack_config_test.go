// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
)

func TestInspectStackConfig(t *testing.T) {
	path := filledStackConfig(t, nil)
	stdout, _, err := runGenerate(t, "inspect", "stack-config", path)
	if err != nil {
		t.Fatal(err)
	}
	want := "STACK_CONFIG='" + path + "'\nSTACK_NICO_ENDPOINT='https://nico.example.com'\nSTACK_NICO_ORG_ID='org-1'\n"
	if stdout != want {
		t.Errorf("got:\n%s\nwant:\n%s", stdout, want)
	}
	// A bad file prints nothing on stdout; the error names the file and
	// every problem.
	stdout, _, err = runGenerate(t, "inspect", "stack-config", stackConfigExample)
	if err == nil || stdout != "" {
		t.Fatalf("the example with its placeholders: %q %v", stdout, err)
	}
	if !strings.HasPrefix(err.Error(), "stack config "+stackConfigExample+":\nnico.endpoint: still a REPLACE_* placeholder; fill it in\nnico.orgID: still") {
		t.Errorf("error: %v", err)
	}
	// The credentials are the install's input too, so an unset one is a
	// problem of the stack config, named as the environment's.
	t.Setenv(stackconfig.EnvNodePassword, "")
	stdout, _, err = runGenerate(t, "inspect", "stack-config", path)
	if err == nil || stdout != "" || !strings.Contains(err.Error(), "environment: "+stackconfig.EnvNodePassword+" is not set") {
		t.Errorf("unset credential: %q %v", stdout, err)
	}
	if _, _, err := runGenerate(t, "inspect", "stack-config", "/nonexistent/stack-config.json"); err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("missing file: %v", err)
	}
}
