// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

// Paths on the node for the failure bundle. The script sits in workDir beside
// the other staged files. Each collection gets a directory and an archive of
// its own, named after the file it writes locally (bundlePaths), so a spec's
// collection and the teardown's never share a path; the sampler writes its
// timeline to a directory of its own, which every collection copies in.
const (
	bundleScriptPath = workDir + "/collect-site-bundle.sh"
	timelineDir      = workDir + "/site-timeline"

	// bundleBusyStatus is the script's own exit status when another
	// collector holds its output directory. A dropped session does not kill
	// the sudo'd script on the node, so a second run on the same paths would
	// otherwise overlap it; the script takes a lock and leaves instead.
	bundleBusyStatus = 73

	// bundleMaxBytes is the largest archive Fetch is allowed to bring back.
	// Fetch holds the whole file in memory, and the script caps every
	// section, so an archive past this is a collector bug rather than
	// evidence.
	bundleMaxBytes = 256 << 20

	// bundleDeadlineMargin is what the budget keeps back from the script's
	// own deadline, for the fetch and the redaction after it.
	bundleDeadlineMargin = 90 * time.Second
	// bundleMinDeadline is the least a script run gets, whatever the budget.
	bundleMinDeadline = time.Minute
	// bundleFetchBudget bounds the fetch, on a context of its own so a run
	// that used the whole budget still gets its archive.
	bundleFetchBudget = 2 * time.Minute
	// bundleKillGrace is how long the EXIT trap has to write the archive
	// after the deadline, before `timeout` kills the shell.
	bundleKillGrace = 30 * time.Second
)

var (
	//go:embed collect-site-bundle.sh
	collectSiteBundleScript string
	// ErrBundleRedaction means the archive was fetched but could not be
	// shown clean, so it was not written.
	ErrBundleRedaction = errors.New("the site bundle could not be redacted")
	// ErrBundleTooLarge means the archive exceeded bundleMaxBytes.
	ErrBundleTooLarge = errors.New("the site bundle is too large")
	// ErrBundleBusy means another collector still holds the directory on the
	// node, so nothing was collected or fetched.
	ErrBundleBusy = errors.New("another collector holds the site bundle directory")
)

// BundleReport is what CollectBundle says about the archive it wrote.
type BundleReport struct {
	// Partial is set when the script hit its deadline or the session dropped
	// while it ran; the archive holds every section that finished.
	Partial bool
	// Interrupted is the manifest's own account: "no", "deadline" or "signal".
	Interrupted string
	// Bytes is the size of the redacted archive.
	Bytes int
	// Sections, Failed and Skipped count index.tsv: every row, the rows whose
	// command exited non-zero, and the rows the script chose not to run.
	Sections, Failed, Skipped int
}

// CollectBundle stages the collector onto the node, runs it under budget,
// fetches the archive it writes, redacts and verifies it, and writes it to
// dest. It reads only, on the node and in the cluster.
//
// Best effort in the same sense as CollectClusterState: the failure that
// prompted it is the finding. The difference is the fail-closed step: an
// archive that cannot be shown free of credentials is not written, because it
// is uploaded as a CI artifact and outlives the node.
func CollectBundle(ctx context.Context, s infra.Session, dest string, budget time.Duration, secrets ...string) (BundleReport, error) {
	return collectBundle(ctx, s, dest, budget, bundleMaxBytes, secrets)
}

func collectBundle(ctx context.Context, s infra.Session, dest string, budget time.Duration, maxBytes int, secrets []string) (BundleReport, error) {
	var report BundleReport
	dir, archive := bundlePaths(dest)
	if err := s.Put(ctx, bundleScriptPath, []byte(collectSiteBundleScript), 0o755); err != nil {
		return report, fmt.Errorf("stage the bundle collector on the node: %w", err)
	}
	// A non-zero status is the deadline (124) or the session going away. The
	// script's EXIT trap has already written the archive from whatever
	// finished, so the run continues to the fetch. The one exception is the
	// script leaving because another collector holds the directory: that
	// archive is the other collector's, so it is not fetched here.
	if _, err := s.Run(ctx, asRoot(bundleCommand(budget, dir, archive))); err != nil {
		if exitedWith(err, bundleBusyStatus) {
			return report, fmt.Errorf("%w: %s", ErrBundleBusy, dir)
		}
		report.Partial = true
	}
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bundleFetchBudget)
	defer cancel()
	data, err := s.Fetch(fetchCtx, archive)
	if err != nil {
		return report, fmt.Errorf("fetch the site bundle: %w", err)
	}
	if len(data) > maxBytes {
		return report, fmt.Errorf("%w: %d bytes", ErrBundleTooLarge, len(data))
	}
	redacted, findings, err := redactBundle(data, secrets)
	if err != nil {
		writeNote(dest+".redaction-failed.txt", "the site bundle was not written: "+err.Error()+"\n")
		return report, fmt.Errorf("%w: %w", ErrBundleRedaction, err)
	}
	if len(findings) > 0 {
		writeNote(dest+".redaction-failed.txt", describeFindings(findings))
		return report, fmt.Errorf("%w: %d finding(s), see %s.redaction-failed.txt", ErrBundleRedaction, len(findings), dest)
	}
	if err := os.WriteFile(dest, redacted, 0o600); err != nil {
		return report, fmt.Errorf("write the site bundle: %w", err)
	}
	report.Bytes = len(redacted)
	fillReport(&report, redacted)
	return report, nil
}

// bundlePaths is where one collection works on the node: the directory, then
// the archive, both under workDir and named after dest's base name, so the
// archive written locally as site-bundle-final.tar.gz was collected in
// site-bundle-final/ and never shares a path with the collection that wrote
// site-bundle.tar.gz.
func bundlePaths(dest string) (string, string) {
	stem := strings.TrimSuffix(filepath.Base(dest), ".tar.gz")
	return workDir + "/" + stem, workDir + "/" + stem + ".tar.gz"
}

// bundleCommand is the one line that runs the staged script as root. One line
// because it goes through sudo's argument joining, which is what eats
// newlines; the script itself traveled by Put and keeps its own.
//
// `timeout` without --foreground signals its whole process group, so the
// script's children get the deadline too; the script traps it, and the kill
// grace is for its EXIT trap to tar what finished.
func bundleCommand(budget time.Duration, dir, archive string) string {
	deadline := max(budget-bundleDeadlineMargin, bundleMinDeadline)
	seconds := int(deadline / time.Second)
	return fmt.Sprintf("SITE_KUBECONFIG=%s SITE_CONTROLLER_NAMESPACE=%s SITE_BUNDLE_TIMELINE_DIR=%s SITE_BUNDLE_DEADLINE_SECONDS=%d timeout -k %d %d sh %s %s %s",
		shellQuote(kubeconfigPath), shellQuote(controller.Namespace()), shellQuote(timelineDir), seconds,
		int(bundleKillGrace/time.Second), seconds,
		shellQuote(bundleScriptPath), shellQuote(dir), shellQuote(archive))
}

// exitedWith reports whether err is the remote command ending with status.
// The ssh package's exit error carries the status; a session that wraps it in
// text, as the fake in the tests does, still prints "exited with status N".
func exitedWith(err error, status int) bool {
	if err == nil {
		return false
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus() == status
	}
	return strings.Contains(err.Error(), fmt.Sprintf("exited with status %d", status))
}

// writeNote leaves a file in the bundle's place saying why there is none.
// Best effort: a note that cannot be written changes nothing about the error
// it would have explained.
func writeNote(path, text string) {
	_ = os.WriteFile(path, []byte(text), 0o600)
}

// fillReport reads index.tsv and manifest.txt out of the archive. Best effort:
// a report that cannot be read leaves the counts at zero and the archive is
// still written.
func fillReport(report *BundleReport, archive []byte) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if err != nil {
			return
		}
		switch {
		case strings.HasSuffix(header.Name, "/index.tsv"):
			content, err := io.ReadAll(tr)
			if err != nil {
				return
			}
			countSections(report, content)
		case strings.HasSuffix(header.Name, "/manifest.txt"):
			content, err := io.ReadAll(tr)
			if err != nil {
				return
			}
			if _, after, ok := strings.Cut(string(content), "interrupted: "); ok {
				report.Interrupted, _, _ = strings.Cut(after, "\n")
			}
		}
	}
}

// countSections tallies index.tsv: a header line, then one row per section
// with the command's exit status in the second column, where `-` marks a
// section the script chose not to run.
func countSections(report *BundleReport, index []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(index))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.SplitN(scanner.Text(), "\t", 3)
		if len(fields) < 2 {
			continue
		}
		report.Sections++
		switch status := fields[1]; {
		case status == "-":
			report.Skipped++
		case status != "0":
			if _, err := strconv.Atoi(status); err == nil || status == "?" {
				report.Failed++
			}
		}
	}
}
