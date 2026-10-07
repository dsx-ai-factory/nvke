// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command summarize-junit reduces Ginkgo JUnit XML to the markdown a GitHub
// job summary shows, so that a run says what it tested without anyone
// downloading an artifact.
//
// It unmarshals into reporters.JUnitTestSuites, the same types Ginkgo wrote
// the file with, so the reader cannot drift from the writer. In return it
// reads only what those types expose as attributes and never the bodies of
// <failure>, <error>, <system-out> or <system-err>: those carry
// CapturedGinkgoWriterOutput, which on the hardware lanes is the output of
// commands run against real machines, and a job summary is a good deal more
// visible than an artifact. -with-text opts into the message attribute alone,
// clipped.
//
// Usage:
//
//	summarize-junit -title "capd-e2e (amd64)" junit.xml junit-nodepool.xml
//
// Writes markdown to stdout. The caller appends it to $GITHUB_STEP_SUMMARY.
// Exits non-zero when a named file cannot be read or parsed; the composite
// action treats that as a warning, because a summary must never be the reason
// a run is red.
package main

import (
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/onsi/ginkgo/v2/reporters"
)

func main() {
	title := flag.String("title", "", "heading for the summary section, naming the lane")
	withText := flag.Bool("with-text", false, "include the failure message attribute of specs that did not pass")
	flag.Parse()

	if *title == "" {
		fmt.Fprintln(os.Stderr, "summarize-junit: -title is required")
		os.Exit(2)
	}

	files := flag.Args()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "summarize-junit: no JUnit files given")
		os.Exit(2)
	}

	if err := run(os.Stdout, *title, *withText, files); err != nil {
		fmt.Fprintf(os.Stderr, "summarize-junit: %v\n", err)
		os.Exit(1)
	}
}

func run(out io.Writer, title string, withText bool, files []string) error {
	var failures []error

	for _, file := range files {
		// One heading per file only when there is more than one. A single-leg
		// lane should not carry a filename it cannot distinguish itself from.
		heading := title
		if len(files) > 1 {
			heading = fmt.Sprintf("%s — %s", title, legName(file))
		}

		markdown, err := summarizeFile(file, heading, withText)
		if err != nil {
			failures = append(failures, err)
			continue
		}

		if _, err := fmt.Fprintf(out, "%s\n", markdown); err != nil {
			return fmt.Errorf("writing the summary: %w", err)
		}
	}

	return errors.Join(failures...)
}

func summarizeFile(path, heading string, withText bool) (string, error) {
	// The path comes from the workflow's own glob over a directory it just
	// wrote, not from anything the suite under test controls.
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}

	var suites reporters.JUnitTestSuites
	if err := xml.Unmarshal(raw, &suites); err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}

	return summarize(suites).markdown(heading, withText), nil
}

// legName turns a report filename into the name of the leg that wrote it, so
// that a two-leg lane's sections are told apart by something meaningful:
// junit-cp-controller.xml becomes "cp-controller". Anything that does not fit
// the convention keeps its filename, which is still unambiguous.
func legName(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	leg := strings.TrimPrefix(base, "junit")
	leg = strings.TrimPrefix(leg, "-")
	if leg == "" {
		return base
	}
	return leg
}
