// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package errors defines nvke-os's coded errors. It follows NVKR's coded-error
// and exit-code contract (NVKR ADR-03): every failure a user can hit carries a
// stable Code, and each Code maps to one distinct, banded process exit status.
// Callers such as NVKR or a CI job branch on the integer, so a published
// status never changes meaning. New codes take the next free integer in their
// band. The shell's 126-165 range is avoided.
//
//	Band   Purpose                                   Allocated
//	0-3    Semantic (non-error)                      success 0, internal 1, usage 2
//	10-19  Input / preflight                         unsupported 10
//	20-29  Build / verify                            (none yet)
//	30-39  Retryable / transient                     (none yet)
//	40-59  Release / repository                      (none yet)
package errors

import (
	"errors"
	"fmt"
)

// Code is a stable, documented error identifier.
type Code string

const (
	// CodeInternal is an unexpected internal failure.
	CodeInternal Code = "NVKE-OS-E-INTERNAL"
	// CodeUsage is invalid command-line usage.
	CodeUsage Code = "NVKE-OS-E-USAGE"
	// CodeUnsupported is a verb or option this build does not implement yet.
	CodeUnsupported Code = "NVKE-OS-E-UNSUPPORTED"
)

// exitCodes is the frozen Code to exit-status map. TestExitCodeContract
// snapshots it.
var exitCodes = map[Code]int{
	CodeInternal:    1,
	CodeUsage:       2,
	CodeUnsupported: 10,
}

// AllCodes lists every declared Code in exit-status order. Go cannot enumerate
// a const block, so a new Code is added here as well as to exitCodes.
var AllCodes = []Code{CodeInternal, CodeUsage, CodeUnsupported}

// Error is a coded, wrappable error.
type Error struct {
	Code Code
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a coded error with a formatted message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Wrap builds a coded error carrying an underlying cause.
func Wrap(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...), Err: cause}
}

// ExitCode returns the process exit status for err: 0 for nil, the mapped
// status for a coded error, and 1 for anything else.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		if code, ok := exitCodes[e.Code]; ok {
			return code
		}
	}
	return 1
}
