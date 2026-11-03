// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTSH writes a tsh that records its arguments and exits with code,
// printing stderr first.
func fakeTSH(t *testing.T, code int, stderr string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	binary := filepath.Join(dir, "tsh")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%s'\nprintf '%%s' '%s' >&2\nexit %d\n", argsFile, stderr, code)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, argsFile
}

func TestCheckJumpHostRunsANoOpThroughTSH(t *testing.T) {
	binary, argsFile := fakeTSH(t, 0, "")
	d := &Dialer{Login: "ubuntu", TSH: binary}
	if err := d.CheckJumpHost(context.Background(), "jump.example"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ssh\nubuntu@jump.example\ntrue\n"; string(got) != want {
		t.Errorf("tsh args = %q, want %q", got, want)
	}
}

func TestCheckJumpHostNamesTheLoginAndTSHsError(t *testing.T) {
	binary, _ := fakeTSH(t, 1, "ERROR: access denied to ubuntu connecting to jump.example")
	d := &Dialer{Login: "ubuntu", TSH: binary}
	err := d.CheckJumpHost(context.Background(), "jump.example")
	if err == nil {
		t.Fatal("want an error from a tsh that fails")
	}
	for _, want := range []string{"as ubuntu", "tsh login", "access denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCheckJumpHostNeedsAJumpHost(t *testing.T) {
	binary, argsFile := fakeTSH(t, 0, "")
	err := (&Dialer{TSH: binary}).CheckJumpHost(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "jumpHost is empty") {
		t.Fatalf("err = %v, want one naming the empty jumpHost", err)
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Error("tsh ran with no jump host to reach")
	}
}

func TestCheckJumpHostSkipsAPlainSSHJump(t *testing.T) {
	binary, argsFile := fakeTSH(t, 1, "")
	d := &Dialer{TSH: binary}
	if err := d.JumpOverSSH(SSHJumpHost{User: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckJumpHost(context.Background(), "jump.example"); err != nil {
		t.Fatalf("a plain SSH jump has nothing for tsh to check, got %v", err)
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Error("tsh ran for a dialer that does not use it")
	}
}
