// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	// annotationPrefix starts a line the runner wrote for a failed step.
	annotationPrefix = "##[error]"
	// contextLines is how many lines of step output before the first
	// annotation are kept. A step that prints its own diagnosis, as the
	// artifact check does, prints it just before it fails.
	contextLines = 12
	// failureLines is how many lines of a Ginkgo failure block are kept.
	failureLines = 6
	// maxLine bounds a single log line. Teardown dumps are long.
	maxLine = 1 << 20
	// maxHeredoc is how many lines of a here-document are skipped before it is
	// taken to be something else and read as output again.
	maxHeredoc = 400
)

var (
	timestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[\d:.]+Z `)
	ansi      = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	// ginkgoLocation is the one-line "[FAILED] in [It] - file:line" that says
	// which spec container failed and where.
	ginkgoLocation = regexp.MustCompile(`^\[FAILED\] in \[\w+\] - `)
	// ginkgoHeader opens the block that says why a spec failed: "[FAILED]"
	// and then a message, as Gomega and Fail print it. The location line is
	// matched first, and a duration such as "[FAILED] [0.001 seconds]" opens
	// with a bracket.
	ginkgoHeader = regexp.MustCompile(`^\[FAILED\] [^\[]`)
	// typeLine is the Go type Gomega prints before an error's message.
	typeLine = regexp.MustCompile(`^<\*[\w.]+ \| 0x[0-9a-f]+>:`)
	// heredoc starts a here-document in the workflow's own shell source,
	// which a step prints before its output. The delimiter is upper case and
	// follows a command, so Ginkgo's "<< End Additional Progress Reports" and a
	// shift such as 1<<20 are not mistaken for one.
	heredoc = regexp.MustCompile(`\S\s*<<-?['"]?([A-Z][A-Z0-9_]*)['"]?(\s|$)`)
)

// Evidence is the part of a job's log that says why it failed.
//
// A job log also holds the workflow's own shell source, so a search of the
// whole log for 403 or EOF matches an echo or a heredoc. Evidence keeps only
// what a failing step wrote: the runner's error annotations, the first Ginkgo
// failure, and the lines before the first annotation.
type Evidence struct {
	// Annotations are the ##[error] lines, less the bare exit-code one.
	Annotations []string
	// Failure is where the first failing spec failed and the block that says
	// why. Later failures are its fallout and are left out.
	Failure []string
	// Context is the step output just before the first annotation.
	Context []string
}

// Text is the evidence as the text a rule is matched against.
func (e Evidence) Text() string {
	var b strings.Builder

	for _, group := range [][]string{e.Annotations, e.Failure, e.Context} {
		for _, l := range group {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}

	return b.String()
}

// scan holds what Extract has seen so far.
type scan struct {
	e         Evidence
	ring      []string
	until     string // the terminator of the here-document being skipped
	skipped   int    // how many lines of it have been skipped
	snapshot  bool
	gotWhere  bool
	gotBlock  bool
	inBlock   bool
	blockSize int
}

// Extract reads a job log and keeps its evidence.
func Extract(r io.Reader) (Evidence, error) {
	var s scan

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	for sc.Scan() {
		s.line(strings.TrimSpace(ansi.ReplaceAllString(timestamp.ReplaceAllString(sc.Text(), ""), "")))
	}

	if err := sc.Err(); err != nil {
		return Evidence{}, fmt.Errorf("reading the job log: %w", err)
	}

	// A step that failed a spec says why in the failure. Its preceding output
	// is the whole suite's, which holds the fallout of later failures too.
	if len(s.e.Failure) > 0 {
		s.e.Context = nil
	}

	return s.e, nil
}

func (s *scan) line(line string) {
	if line == "" {
		// A blank line ends a failure block that has said something.
		if s.inBlock && s.blockSize > 1 {
			s.endBlock()
		}

		return
	}

	if s.until != "" {
		s.skipped++
		if line == s.until || s.skipped > maxHeredoc {
			s.until = ""
		}

		return
	}

	if m := heredoc.FindStringSubmatch(line); m != nil && !strings.HasPrefix(line, "##[") {
		s.until, s.skipped = m[1], 0

		return
	}

	switch {
	case strings.HasPrefix(line, annotationPrefix):
		if !s.snapshot {
			s.e.Context = append(s.e.Context, s.ring...)
			s.snapshot = true
		}

		if msg := strings.TrimPrefix(line, annotationPrefix); !strings.HasPrefix(msg, "Process completed with exit code") {
			s.e.Annotations = append(s.e.Annotations, msg)
		}
	case ginkgoLocation.MatchString(line):
		if !s.gotWhere {
			s.e.Failure = append(s.e.Failure, line)
			s.gotWhere = true
		}
	case ginkgoHeader.MatchString(line):
		if !s.gotBlock && !s.inBlock {
			s.e.Failure = append(s.e.Failure, line)
			s.inBlock, s.blockSize = true, 1
		}
	case s.inBlock:
		if strings.HasPrefix(line, "In [") || line == "occurred" || s.blockSize >= failureLines {
			s.endBlock()

			break
		}

		if !typeLine.MatchString(line) {
			s.e.Failure = append(s.e.Failure, line)
			s.blockSize++
		}
	}

	if !s.snapshot && !strings.Contains(line, "::error::") && !strings.HasPrefix(line, "##[") && !strings.HasPrefix(line, "echo ") {
		s.ring = append(s.ring, line)
		if len(s.ring) > contextLines {
			s.ring = s.ring[1:]
		}
	}
}

func (s *scan) endBlock() {
	s.inBlock, s.gotBlock = false, true
}
