// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// FatalError is the panic value used by MainTB.Fatalf to unwind the stack.
// Callers should recover this and treat it as a test failure (exit code 1).
type FatalError struct{ Msg string }

func (e FatalError) Error() string { return e.Msg }

// SkipError is the panic value used by MainTB.Skip/Skipf.
// Callers should recover this and treat it as a skip (exit code 0).
type SkipError struct{ Msg string }

func (e SkipError) Error() string { return e.Msg }

// MainTB implements testing.TB for use in TestMain where no *testing.T is
// available. Fatal/Skip abort via panic (FatalError/SkipError); the caller
// must recover. The embedded testing.TB satisfies the unexported private()
// method required by the interface.
type MainTB struct {
	testing.TB
	name     string
	failed   bool
	skipped  bool
	cleanups []func()
	tempDir  string
}

func NewMainTB(name string) *MainTB {
	return &MainTB{name: name}
}

func (tb *MainTB) Helper() {}

func (tb *MainTB) Name() string { return tb.name }

func (tb *MainTB) Failed() bool { return tb.failed }

func (tb *MainTB) Skipped() bool { return tb.skipped }

func (tb *MainTB) log(msg string) {
	_, file, line, ok := runtime.Caller(2)
	if ok {
		file = filepath.Base(file)
	} else {
		file, line = "???", 0
	}
	fmt.Fprintf(os.Stderr, "    %s:%d: %s\n", file, line, msg)
}

func (tb *MainTB) Log(args ...any) {
	tb.log(fmt.Sprint(args...))
}

func (tb *MainTB) Logf(format string, args ...any) {
	tb.log(fmt.Sprintf(format, args...))
}

func (tb *MainTB) Error(args ...any) {
	tb.Log(args...)
	tb.failed = true
}

func (tb *MainTB) Errorf(format string, args ...any) {
	tb.Logf(format, args...)
	tb.failed = true
}

func (tb *MainTB) Fail() {
	tb.failed = true
}

func (tb *MainTB) FailNow() {
	tb.failed = true
	panic(FatalError{Msg: "FailNow called"})
}

func (tb *MainTB) Fatal(args ...any) {
	tb.Log(args...)
	tb.failed = true
	panic(FatalError{Msg: fmt.Sprint(args...)})
}

func (tb *MainTB) Fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	tb.Logf("%s", msg)
	tb.failed = true
	panic(FatalError{Msg: msg})
}

func (tb *MainTB) Skip(args ...any) {
	tb.Log(args...)
	tb.skipped = true
	panic(SkipError{Msg: fmt.Sprint(args...)})
}

func (tb *MainTB) Skipf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	tb.Logf("%s", msg)
	tb.skipped = true
	panic(SkipError{Msg: msg})
}

func (tb *MainTB) SkipNow() {
	tb.skipped = true
	panic(SkipError{Msg: "SkipNow called"})
}

func (tb *MainTB) Cleanup(f func()) {
	tb.cleanups = append(tb.cleanups, f)
}

// RunCleanups runs all registered cleanup functions in LIFO order.
// Safe to call multiple times; subsequent calls are no-ops.
func (tb *MainTB) RunCleanups() {
	for i := len(tb.cleanups) - 1; i >= 0; i-- {
		tb.cleanups[i]()
	}
	tb.cleanups = nil
}

func (tb *MainTB) TempDir() string {
	if tb.tempDir == "" {
		dir, err := os.MkdirTemp("", "forge-vms-testmain-*")
		if err != nil {
			tb.Fatalf("create temp dir: %v", err)
		}
		tb.tempDir = dir
		tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	return tb.tempDir
}

func (tb *MainTB) Setenv(key, value string) {
	prev, existed := os.LookupEnv(key)
	_ = os.Setenv(key, value)
	tb.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
