// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package darwinqemu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type blockedProcessOutput struct {
	closing chan struct{}
	release chan struct{}
}

func (output *blockedProcessOutput) Close() error {
	close(output.closing)
	<-output.release
	return nil
}

func TestNativeStopBoundsUnjoinedFinalization(t *testing.T) {
	output := &blockedProcessOutput{closing: make(chan struct{}), release: make(chan struct{})}
	process, err := startOwnedProcess(exec.Command("/usr/bin/true"), output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(output.release)
		select {
		case <-process.done:
		case <-time.After(2 * time.Second):
			t.Error("test-owned process finalizer did not join after release")
		}
	})
	select {
	case <-output.closing:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not enter output finalization")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	err = process.stop(ctx)
	if !errors.Is(err, errNativeProcessUnjoined) || !errors.Is(err, context.Canceled) {
		t.Fatalf("unjoined output finalization was misreported: %v", err)
	}
	if time.Since(started) > nativeKillJoinTimeout+2*time.Second {
		t.Fatal("native teardown exceeded its post-kill join bound")
	}
	select {
	case <-process.done:
		t.Fatal("blocked finalization was claimed joined")
	default:
	}
}

func TestSavedPIDCannotAuthorizeNativeProcess(t *testing.T) {
	foreign, err := startOwnedProcess(exec.Command("/bin/sleep", "30"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := foreign.stop(ctx); err != nil {
			t.Error(err)
		}
	})
	r := &darwinQEMU{instancesDir: t.TempDir()}
	dir := filepath.Join(r.instancesDir, "foreign")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qemu.pid"), []byte(strconv.Itoa(foreign.command.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetState(t.Context(), "foreign"); err == nil {
		t.Fatal("saved PID authorized status")
	}
	if err := r.Reboot(t.Context(), "foreign"); err == nil {
		t.Fatal("saved PID authorized reboot")
	}
	if err := r.Delete(t.Context(), "foreign"); err == nil {
		t.Fatal("saved PID authorized deletion")
	}
	if machines, err := r.ListMachines(t.Context()); err != nil || len(machines) != 0 {
		t.Fatalf("saved PID admitted to owned machines: %v, %v", machines, err)
	}
	select {
	case <-foreign.done:
		t.Fatal("foreign process was terminated")
	default:
	}
}

func TestCanceledTeardownJoinsEveryOwnedChild(t *testing.T) {
	r := &darwinQEMU{machines: make(map[string]*ownedProcess)}
	for _, id := range []string{"first", "second"} {
		process, err := startOwnedProcess(exec.Command("/bin/sleep", "30"), nil)
		if err != nil {
			t.Fatal(err)
		}
		r.machines[id] = process
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = process.stop(ctx)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.TeardownNetwork(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled teardown error = %v", err)
	}
	for id, process := range r.machines {
		select {
		case <-process.done:
		default:
			t.Fatalf("owned child %s was left running after canceled teardown", id)
		}
	}
}

func TestNativeDeleteJoinsOwnedProcess(t *testing.T) {
	process, err := startOwnedProcess(exec.Command("/bin/sleep", "30"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = process.stop(ctx) })
	r := &darwinQEMU{instancesDir: t.TempDir(), machines: map[string]*ownedProcess{"owned": process}}
	if err := r.Delete(ctx, "owned"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
	default:
		t.Fatal("deletion returned before joining the owned process")
	}
	if machines, err := r.ListMachines(ctx); err != nil || len(machines) != 0 {
		t.Fatalf("deleted process remains owned: %v, %v", machines, err)
	}
	if err := r.Delete(ctx, "owned"); err != nil {
		t.Fatalf("repeated deletion: %v", err)
	}
}
