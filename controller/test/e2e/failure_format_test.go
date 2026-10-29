// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	"github.com/onsi/gomega/format"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func init() {
	format.RegisterCustomFormatter(formatError)
}

// forbiddenHint follows an error the API server refused on authorization.
const forbiddenHint = "(the identity the suite runs as is not allowed this; for the validation image, " +
	"see the RBAC it needs in controller/test/e2e/README.md)"

// failureCauseEntry names the report entry that carries a failure's cause:
// the text of the error the failing assertion was about. Ginkgo writes report
// entries into the JUnit report as test-case properties, and `qualify publish`
// shows this one on TestGrid, which keeps too little of a failure message for
// both Gomega's framing and the cause to fit.
const failureCauseEntry = "failure-cause"

// maxFormattedErrors bounds formattedErrors: Gomega also formats errors for
// the progress reports of a long Eventually, and no failure clears those.
const maxFormattedErrors = 32

// formattedErrors are the errors Gomega has formatted since the last failure,
// oldest first, with when each was formatted. Gomega formats an error only to
// describe a failure, a failed attempt of an Eventually, or a progress
// report, so the error a failure is about is among them. Progress reports are
// written from another goroutine, hence the lock.
var formattedErrors struct {
	sync.Mutex
	errs []formattedError
}

// formattedError is an error Gomega formatted, and when it did.
type formattedError struct {
	err error
	at  time.Time
}

// takeFormattedErrors empties formattedErrors and returns the errors it held
// that were formatted at or after since. failWithCause passes the start of
// the failing spec, so an error formatted during an earlier spec, for a
// progress report or an attempt that later passed, is not taken for this
// one's cause.
func takeFormattedErrors(since time.Time) []formattedError {
	formattedErrors.Lock()
	defer formattedErrors.Unlock()
	var errs []formattedError
	for _, f := range formattedErrors.errs {
		if !f.at.Before(since) {
			errs = append(errs, f)
		}
	}
	formattedErrors.errs = nil
	return errs
}

// formatError stands in for the fields Gomega prints after an error's
// message, which repeat the message once per wrapped layer and bury it. It
// prints nothing there but, for a request the API server refused on
// authorization, where to look. It also remembers the error, for
// failWithCause.
func formatError(value any) (string, bool) {
	err, ok := value.(error)
	if !ok || err == nil {
		return "", false
	}
	formattedErrors.Lock()
	formattedErrors.errs = append(formattedErrors.errs, formattedError{err: err, at: time.Now()})
	if n := len(formattedErrors.errs); n > maxFormattedErrors {
		formattedErrors.errs = formattedErrors.errs[n-maxFormattedErrors:]
	}
	formattedErrors.Unlock()
	if apierrors.IsForbidden(err) {
		return forbiddenHint, true
	}
	return "", true
}

// failWithCause is the suite's fail handler: Ginkgo's Fail, after recording
// the failure's cause as a report entry when there is one. A failure that is
// not about an error, such as a value mismatch, records nothing.
func failWithCause(message string, callerSkip ...int) {
	skip := 0
	if len(callerSkip) > 0 {
		skip = callerSkip[0]
	}
	report := CurrentSpecReport()
	errs := takeFormattedErrors(report.StartTime)
	// Report entries can be added only while specs run, not while the spec
	// tree is built, and a refused one would fail in place of this failure.
	if cause := failureCause(message, errs); cause != "" && report.LeafNodeType != types.NodeTypeInvalid {
		// Located at the failing assertion, as the failure itself is.
		AddReportEntry(failureCauseEntry, cause, types.NewCodeLocation(skip+1), ReportEntryVisibilityNever)
	}
	Fail(message, skip+1)
}

// maxCauseBytes bounds a recorded cause. The whole text is already in the
// failure message and the timeline, and a summary needs far less of it.
const maxCauseBytes = 4096

// causeWindow is how long before the newest error a failure message carries
// the message's other errors can have been formatted. Gomega formats them as
// it builds the message, microseconds apart; the rest is room for a slow Error
// method or a pause between them. An earlier attempt of an Eventually
// formatted its errors at least a polling interval before, and this suite
// polls every 200ms or slower. A faster poll, or a progress report on a
// different error, could put an error the failure is not about inside the
// window.
const causeWindow = 100 * time.Millisecond

// failureCause is the text of the error the failure message is about, cut at a
// word to at most maxCauseBytes, or "" when there is none. The newest error
// the message contains is one Gomega formatted for it: as it built the
// message or, for an Eventually polling a function that takes a Gomega and
// returns nothing, which formats nothing when it gives up, in its last
// attempt. The cause is the first error the message contains among those
// formatted up to causeWindow before that one, since Gomega formats an
// assertion's actual value before its expected one. Whitespace is not
// compared, since Gomega indents each line of an error.
func failureCause(message string, errs []formattedError) string {
	flat := oneLine(message)
	// texts holds the text of each error the message contains, "" for the rest.
	texts := make([]string, len(errs))
	newest := -1
	for i, f := range errs {
		text := errorText(f.err)
		if t := oneLine(text); t != "" && strings.Contains(flat, t) {
			texts[i], newest = text, i
		}
	}
	for i := range newest + 1 {
		if texts[i] != "" && errs[newest].at.Sub(errs[i].at) <= causeWindow {
			return truncateAtWord(texts[i], maxCauseBytes)
		}
	}
	return ""
}

// truncateAtWord cuts s to at most n bytes at the end of a word, so that a
// word such as a host name is kept whole or left out, never split: the
// workflow withholds such values by matching them whole. For the same reason
// it gives "" when no word ends within n bytes.
func truncateAtWord(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := truncateToBytes(s, n)
	if next, _ := utf8.DecodeRuneInString(s[len(cut):]); unicode.IsSpace(next) {
		return cut
	}
	if i := strings.LastIndexFunc(cut, unicode.IsSpace); i >= 0 {
		return strings.TrimRightFunc(cut[:i], unicode.IsSpace)
	}
	return ""
}

// errorText is err.Error(), or "" when that panics, as it can for a nil
// pointer of an error type: Gomega does not call it on those, and the fail
// handler must not turn a failure into a panic.
func errorText(err error) string {
	var text string
	func() {
		defer func() {
			if recover() != nil {
				text = ""
			}
		}()
		text = err.Error()
	}()
	return text
}

// oneLine is s with each run of whitespace, line breaks included, made a
// single space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateToBytes cuts s to at most n bytes without splitting a UTF-8
// sequence.
func truncateToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
