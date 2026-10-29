// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// formattedAt is errs as Gomega formats them for one message, microseconds
// apart, starting ago before now.
func formattedAt(now time.Time, ago time.Duration, errs ...error) []formattedError {
	formatted := make([]formattedError, 0, len(errs))
	for i, err := range errs {
		formatted = append(formatted, formattedError{err: err, at: now.Add(-ago + time.Duration(i)*time.Microsecond)})
	}
	return formatted
}

func TestUnit_FailureCause(t *testing.T) {
	now := time.Now()
	// own is errors formatted for the failing message itself; earlier is errors
	// formatted seconds before, for an earlier attempt of an Eventually or a
	// progress report.
	own := func(errs ...error) []formattedError { return formattedAt(now, 0, errs...) }
	earlier := func(errs ...error) []formattedError { return formattedAt(now, 5*time.Second, errs...) }
	cause := func(message string, groups ...[]formattedError) string {
		var errs []formattedError
		for _, group := range groups {
			errs = append(errs, group...)
		}
		return failureCause(message, errs)
	}

	quota := fmt.Errorf("allocating machines: %w", errors.New("403 Forbidden\n{\"error\":\"project quota exceeded\"}"))
	message := "provisioning the site\nUnexpected error:\n    <*fmt.wrapError | 0xc000123456>: \n" +
		"    allocating machines: 403 Forbidden\n    {\"error\":\"project quota exceeded\"}\n    \noccurred"
	require.Equal(t, quota.Error(), cause(message, own(quota)), "the error the message is about, whole")
	require.Empty(t, cause(message, own(errors.New("cluster c1 has 1 control-plane nodes KubernetesReady; want 3"))),
		"no error the message is about")
	require.Empty(t, cause(message))
	require.Empty(t, cause(message, own(errors.New(" \n "))), "an error with no text is no cause")
	var nilPointer *panickingError
	require.Empty(t, cause(message, own(nilPointer)), "an Error method that panics gives no cause, and no panic")

	refused := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	unexpected := "Unexpected error:\n    <*errors.errorString | 0xc000123456>: \n" +
		"    dial tcp 10.0.0.1:443: connect: connection refused\n    \noccurred"
	require.Equal(t, refused.Error(), cause(unexpected, earlier(errors.New("error")), own(refused)),
		"not an error formatted earlier, though Gomega's own wording contains its text")

	listing := fmt.Errorf("listing machines: %w", context.DeadlineExceeded)
	contextDone := "Context was cancelled (cause: context deadline exceeded) after 600.000s.\n" + //nolint:misspell // Gomega's own wording
		"Expected success, but got an error:\n    <*fmt.wrapError | 0xc000123456>: \n    listing machines: context deadline exceeded\n    "
	require.Equal(t, listing.Error(), cause(contextDone, earlier(context.DeadlineExceeded), own(listing)),
		"not an earlier error whose text Gomega's wording repeats")

	// An Eventually polling a function that takes a Gomega formats each
	// attempt's error as it fails, and nothing when it gives up.
	eof := errors.New("EOF")
	readingBody := fmt.Errorf("reading the body: %w", errors.New("unexpected EOF"))
	timedOut := "Timed out after 600.000s.\nThe function passed to Eventually failed at /e2e/a_test.go:12 with:\n" +
		"Unexpected error:\n    <*fmt.wrapError | 0xc000123456>: \n    reading the body: unexpected EOF\n    \noccurred"
	require.Equal(t, readingBody.Error(), cause(timedOut, formattedAt(now, 15*time.Second, eof),
		formattedAt(now, 10*time.Second, eof), formattedAt(now, 5*time.Second, readingBody)),
		"the last attempt's error, not a fragment of it from an earlier attempt")
	require.Equal(t, readingBody.Error(), cause(timedOut, formattedAt(now, 5*time.Second, readingBody),
		own(errors.New("watching nodes: stream closed"))),
		"not an error formatted later that the message does not carry")

	denied := errors.New("permission denied")
	notFound := errors.New("not found")
	mismatched := "Expected\n    <*errors.errorString | 0xc000123456>: \n    permission denied\n    \n" +
		"to match error\n    <*errors.errorString | 0xc000654321>: \n    not found\n    "
	require.Equal(t, denied.Error(), cause(mismatched, own(denied, notFound)),
		"the actual error, formatted first, rather than the expected one formatted after it")
	require.Equal(t, denied.Error(), cause(mismatched, formattedAt(now, 10*time.Second, denied, notFound),
		formattedAt(now, 5*time.Second, denied, notFound)),
		"the actual error of an Eventually's last attempt, seconds before it gives up")

	// Expect(io.EOF).To(MatchError(io.ErrUnexpectedEOF)): the expected error's
	// text contains the actual one's.
	unexpectedEOF := errors.New("unexpected EOF")
	matched := "Expected\n    <*errors.errorString | 0xc000123456>: \n    EOF\n    \n" +
		"to match error\n    <*errors.errorString | 0xc000654321>: \n    unexpected EOF\n    "
	require.Equal(t, eof.Error(), cause(matched, own(eof, unexpectedEOF)),
		"the actual error, even when the expected one's text contains it")
	require.Equal(t, eof.Error(), cause(matched, formattedAt(now, 50*time.Millisecond, eof), own(unexpectedEOF)),
		"the actual error, even when a slow Error method or a pause comes between the two")

	long := errors.New(strings.Repeat("ノード ", maxCauseBytes))
	cut := cause(long.Error(), own(long))
	require.LessOrEqual(t, len(cut), maxCauseBytes)
	require.True(t, utf8.ValidString(cut), "cut on a rune boundary")
	require.True(t, strings.HasPrefix(long.Error(), cut))
	require.True(t, strings.HasPrefix(long.Error()[len(cut):], " "), "cut at the end of a word")

	straddling := errors.New(strings.Repeat("a", maxCauseBytes-5) + " jump-host.example.internal not reachable")
	cut = cause(straddling.Error(), own(straddling))
	require.NotContains(t, cut, "jump", "a word the cut would split is left out whole")
	require.True(t, strings.HasPrefix(straddling.Error(), cut))

	oneWord := errors.New(strings.Repeat("a", maxCauseBytes-6) + "jump-host.example.internal")
	require.Empty(t, cause(oneWord.Error(), own(oneWord)), "a word longer than the limit is not cut, so nothing is recorded")
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

	// Both of a MatchError's errors, actual first, as Gomega builds one
	// message. The actual one is the cause, unless the test paused for longer
	// than causeWindow between the two.
	g.Expect(io.EOF).To(gomega.MatchError(io.ErrUnexpectedEOF))
	errs := takeFormattedErrors(time.Time{})
	require.Len(t, errs, 2, "message: %q", message)
	require.Equal(t, []error{io.EOF, io.ErrUnexpectedEOF}, []error{errs[0].err, errs[1].err})
	want := io.EOF.Error()
	if errs[1].at.Sub(errs[0].at) > causeWindow {
		want = io.ErrUnexpectedEOF.Error()
	}
	require.Equal(t, want, failureCause(message, errs), "message: %q", message)

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

	require.Equal(t, []formattedError{{err: current, at: specStart}}, takeFormattedErrors(specStart),
		"an error formatted before the failing spec began is not one of its causes")
	require.Empty(t, takeFormattedErrors(time.Time{}), "taking empties the buffer, earlier errors included")
}
