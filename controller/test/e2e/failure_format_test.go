// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"sync"
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
// oldest first. Gomega formats an error only to describe a failure or for a
// progress report, so the error a failure is about is among them. Progress
// reports are written from another goroutine, hence the lock.
var formattedErrors struct {
	sync.Mutex
	errs []error
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
	formattedErrors.errs = append(formattedErrors.errs, err)
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
	formattedErrors.Lock()
	errs := formattedErrors.errs
	formattedErrors.errs = nil
	formattedErrors.Unlock()
	// Report entries can be added only while specs run, not while the spec
	// tree is built, and a refused one would fail in place of this failure.
	if cause := failureCause(message, errs); cause != "" && CurrentSpecReport().LeafNodeType != types.NodeTypeInvalid {
		// Located at the failing assertion, as the failure itself is.
		AddReportEntry(failureCauseEntry, cause, types.NewCodeLocation(skip+1), ReportEntryVisibilityNever)
	}
	Fail(message, skip+1)
}

// maxCauseBytes bounds a recorded cause. The whole text is already in the
// failure message and the timeline, and a summary needs far less of it.
const maxCauseBytes = 4096

// failureCause is the text of the error the failure message is about, cut to
// maxCauseBytes, or "" when the message carries none of errs. That is the
// first of errs whose text the message contains, or a later one whose text
// contains that one's in turn: an error formatted earlier, for a progress
// report or an earlier attempt of an Eventually, can be a fragment of the
// final one, such as "EOF" before "reading the body: unexpected EOF".
// Whitespace is not compared, since Gomega indents each line of an error.
func failureCause(message string, errs []error) string {
	flat := oneLine(message)
	cause, causeFlat := "", ""
	for _, err := range errs {
		text := errorText(err)
		textFlat := oneLine(text)
		if textFlat == "" || !strings.Contains(flat, textFlat) {
			continue
		}
		if causeFlat == "" || (textFlat != causeFlat && strings.Contains(textFlat, causeFlat)) {
			cause, causeFlat = text, textFlat
		}
	}
	return truncateToBytes(cause, maxCauseBytes)
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
