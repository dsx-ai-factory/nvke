// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestUnit_FormatError(t *testing.T) {
	_, ok := formatError("not an error")
	require.False(t, ok)
	_, ok = formatError(error(nil))
	require.False(t, ok)

	wrapped := fmt.Errorf("listing nodes: %w", fmt.Errorf("through the relay: %w", errors.New("connection refused")))
	printed := format.Object(wrapped, 1)
	require.Equal(t, 1, strings.Count(printed, "connection refused"), "the message once, not each wrapper's fields: %q", printed)
	require.NotContains(t, printed, forbiddenHint)

	forbidden := fmt.Errorf("reading the tenant credential: %w",
		apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "nvke-validate-blue", errors.New("no RBAC")))
	printed = format.Object(forbidden, 1)
	require.Equal(t, 1, strings.Count(printed, "no RBAC"), "the message once: %q", printed)
	require.Contains(t, printed, forbiddenHint, "a refused request points at the RBAC")
}

func TestUnit_FailureCause(t *testing.T) {
	quota := fmt.Errorf("allocating machines: %w", errors.New("403 Forbidden\n{\"error\":\"project quota exceeded\"}"))
	message := "provisioning the site\nUnexpected error:\n    <*fmt.wrapError | 0xc000123456>: \n" +
		"    allocating machines: 403 Forbidden\n    {\"error\":\"project quota exceeded\"}\n    \noccurred"
	earlier := errors.New("cluster c1 has 1 control-plane nodes KubernetesReady; want 3")

	require.Equal(t, quota.Error(), failureCause(message, []error{earlier, quota}),
		"the error the message is about, passing over one formatted earlier for a progress report")
	require.Empty(t, failureCause(message, []error{earlier}), "no error the message is about")
	require.Empty(t, failureCause(message, nil))
	require.Empty(t, failureCause(message, []error{errors.New(" \n ")}), "an error with no text is no cause")

	eof := errors.New("EOF")
	readingBody := fmt.Errorf("reading the body: %w", errors.New("unexpected EOF"))
	timedOut := "Timed out after 600.000s.\nThe function passed to Eventually failed at /e2e/a_test.go:12 with:\n" +
		"Unexpected error:\n    <*fmt.wrapError | 0xc000123456>: \n    reading the body: unexpected EOF\n    \noccurred"
	require.Equal(t, readingBody.Error(), failureCause(timedOut, []error{eof, readingBody}),
		"a later error that says more than an earlier fragment of it")

	denied := errors.New("permission denied")
	notFound := errors.New("not found")
	mismatched := "Expected\n    <*errors.errorString | 0xc000123456>: \n    permission denied\n    \n" +
		"to match error\n    <*errors.errorString | 0xc000654321>: \n    not found\n    "
	require.Equal(t, denied.Error(), failureCause(mismatched, []error{denied, notFound}),
		"the actual error, formatted first, rather than the expected one formatted after it")

	// Expect(io.EOF).To(MatchError(io.ErrUnexpectedEOF)): the expected error's
	// text contains the actual one's, which also stands on its own.
	unexpectedEOF := errors.New("unexpected EOF")
	matched := "Expected\n    <*errors.errorString | 0xc000123456>: \n    EOF\n    \n" +
		"to match error\n    <*errors.errorString | 0xc000654321>: \n    unexpected EOF\n    "
	require.Equal(t, eof.Error(), failureCause(matched, []error{eof, unexpectedEOF}),
		"the actual error, even when the expected one's text contains it")

	var nilPointer *panickingError
	require.Empty(t, failureCause(message, []error{nilPointer}), "an Error method that panics gives no cause, and no panic")

	long := errors.New(strings.Repeat("ノード ", maxCauseBytes))
	cut := failureCause(long.Error(), []error{long})
	require.LessOrEqual(t, len(cut), maxCauseBytes)
	require.True(t, utf8.ValidString(cut), "cut on a rune boundary")
	require.True(t, strings.HasPrefix(long.Error(), cut))
	require.True(t, strings.HasPrefix(long.Error()[len(cut):], " "), "cut at the end of a word")

	straddling := errors.New(strings.Repeat("a", maxCauseBytes-5) + " jump-host.example.internal not reachable")
	cut = failureCause(straddling.Error(), []error{straddling})
	require.NotContains(t, cut, "jump", "a word the cut would split is left out whole")
	require.True(t, strings.HasPrefix(straddling.Error(), cut))
}

// panickingError dereferences its receiver, so a nil pointer of it panics in
// Error.
type panickingError struct{ reason string }

func (e *panickingError) Error() string { return e.reason }

func TestUnit_FormatErrorIsSafeAcrossGoroutines(t *testing.T) {
	takeFormattedErrors(time.Time{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				formatError(fmt.Errorf("progress report %d.%d", i, j))
				if j%10 == 0 {
					takeFormattedErrors(time.Time{})
				}
			}
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, len(takeFormattedErrors(time.Time{})), maxFormattedErrors)
}

func TestUnit_FormatErrorRemembersTheErrorsItFormats(t *testing.T) {
	takeFormattedErrors(time.Time{})

	// The message a failing Gomega assertion builds, with this suite's
	// formatter registered.
	var message string
	g := gomega.NewGomega(func(m string, _ ...int) { message = m })
	cause := fmt.Errorf("reaching NVKE over the port-forward: %w", errors.New("connection refused"))
	g.Expect(cause).NotTo(gomega.HaveOccurred(), "listing clusters")
	require.Equal(t, cause.Error(), failureCause(message, takeFormattedErrors(time.Time{})), "message: %q", message)

	for i := range 2 * maxFormattedErrors {
		formatError(fmt.Errorf("progress %d", i))
	}
	require.Len(t, takeFormattedErrors(time.Time{}), maxFormattedErrors, "formatted errors are bounded")
}

func TestUnit_TakeFormattedErrorsPassesOverEarlierSpecs(t *testing.T) {
	specStart := time.Now()
	earlier := errors.New("connection refused")
	current := errors.New("node n1 not found")
	formattedErrors.Lock()
	formattedErrors.errs = []formattedError{{err: earlier, at: specStart.Add(-time.Second)}, {err: current, at: specStart}}
	formattedErrors.Unlock()

	require.Equal(t, []error{current}, takeFormattedErrors(specStart),
		"an error formatted before the failing spec began is not one of its causes")
	require.Empty(t, takeFormattedErrors(time.Time{}), "taking empties the buffer, earlier errors included")
}
