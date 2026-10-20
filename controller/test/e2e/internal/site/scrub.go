// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// A bundle is a journal, a cluster dump and a node's configuration, uploaded
// as a CI artifact. redactArchive removes the values the run knows it wrote
// onto the node. This file removes the shapes a credential takes when the run
// did not write it: a kubeadm token in a join string, a bearer token in a log
// line, a private key in a ConfigMap someone put there. Each pattern is
// applied, and then every pattern and every known value is searched for again
// over the result; anything still found fails the bundle closed, and the suite
// writes a note instead of the archive.
//
// The replacements are chosen so that a pattern cannot match its own output:
// a value becomes `***`, which is shorter than any value a pattern accepts.

// scrubPattern is one shape of credential and what replaces it.
type scrubPattern struct {
	name        string
	re          *regexp.Regexp
	replacement string
}

const (
	// keepKey is the replacement for a pattern whose first group is the key
	// or flag that introduced the value: the key stays, the value becomes
	// `***`, which is shorter than any value the patterns accept.
	keepKey = "${1}***"
)

// scrubPatterns compiles the table. A function rather than a package variable
// so the table is built where it is used; it is a handful of expressions and
// a bundle is collected at most twice a run.
func scrubPatterns() []scrubPattern {
	return []scrubPattern{
		{name: "kubeadm-token", re: regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`), replacement: "REDACTED-TOKEN"},
		{name: "certificate-key", re: regexp.MustCompile(`(--certificate-key[=[:space:]]+)[0-9a-fA-F]{64}`), replacement: keepKey},
		{name: "ca-cert-hash", re: regexp.MustCompile(`(discovery-token-ca-cert-hash[=[:space:]]+sha256:)[0-9a-fA-F]{64}`), replacement: keepKey},
		{
			name: "keyed-value",
			re: regexp.MustCompile(`(?i)((?:password|passwd|pwd|api[_-]?key|access[_-]?key|secret[_-]?key|client[_-]?secret|join[_-]?token|spireJoinToken|` +
				`certificate[_-]?key|api[_-]?encryption[_-]?key|client-key-data|client-certificate-data|identitytoken|registrytoken|` +
				// `auth` only at the start of a word: containerd's registry
				// `auth = "<base64>"`, not the tail of `oauth`.
				`(?:^|[^a-z])auth)["']?[ \t]*[:=][ \t]*["']?)[^"'[:space:],]{8,}`),
			replacement: keepKey,
		},
		// A registry's HTTP Basic credential: containerd's hosts.toml carries
		// it as `Authorization = ["Basic <base64>"]` under a [header] table or
		// inline, a request log as `Authorization: Basic <base64>`. The value
		// is base64(user:password), which the known-value needles do not
		// hold even when the password is one of them.
		{
			name:        "basic-auth",
			re:          regexp.MustCompile(`(?i)((?:proxy-)?authorization["']?[ \t]*[:=][ \t]*\[?[ \t]*["']?basic[ \t]+)[A-Za-z0-9+/=]{8,}`),
			replacement: keepKey,
		},
		// The same credential with its header name out of sight: a long
		// base64 run after the word is a credential far more often than prose.
		{name: "basic-credential", re: regexp.MustCompile(`(?i)(\bbasic[ \t]+)[A-Za-z0-9+/=]{16,}`), replacement: keepKey},
		// No colon in the value: a token never has one, and an empty `Token:`
		// field in a struct dump is followed by the next `Key:value`, which is
		// not the token.
		{name: "token-value", re: regexp.MustCompile(`(?i)(\btoken["']?[ \t]*[:=][ \t]*["']?)[^"'[:space:],:]{16,}`), replacement: keepKey},
		{name: "join-token-flag", re: regexp.MustCompile(`(-joinToken[=[:space:]]+)[^[:space:]*]+`), replacement: keepKey},
		{name: "bearer", re: regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{20,}`), replacement: keepKey},
		{name: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), replacement: "REDACTED-JWT"},
		{name: "url-password", re: regexp.MustCompile(`((?:postgres(?:ql)?|mysql|amqps?|rediss?|https?)://[^:/[:space:]@"']+:)[^@/[:space:]"'*]+(@)`), replacement: "${1}***${2}"},
		{name: "nvapi-key", re: regexp.MustCompile(`nvapi-[A-Za-z0-9_-]{20,}`), replacement: "nvapi-REDACTED"},
		{
			name:        "pem-private-key",
			re:          regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
			replacement: "[PEM private key redacted by the e2e suite]",
		},
	}
}

// deniedMembers are archive member names that must never be present, whatever
// their contents: the collector does not read them, and this is the check
// that it did not. Matched against the member name with its directory.
func deniedMembers() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(^|/)(admin|super-admin|controller-manager|scheduler|kubelet)\.conf$`),
		regexp.MustCompile(`(^|/)[^/]*kubeconfig[^/]*$`),
		regexp.MustCompile(`(^|/)pki/`),
		regexp.MustCompile(`(^|/)spire(-agent)?\.env$`),
		regexp.MustCompile(`(^|/)sitespec\.yaml$`),
		regexp.MustCompile(`(^|/)values/`),
		regexp.MustCompile(`(^|/)\.tsh(/|$)`),
		regexp.MustCompile(`(^|/)teleport/`),
	}
}

// scrubFinding is one thing verifyClean found: where, and which pattern or
// that a known value was still present. Never the value itself, because the
// finding is written to a note that is uploaded in the bundle's place.
type scrubFinding struct {
	member string
	what   string
}

func (f scrubFinding) String() string { return f.member + ": " + f.what }

// scrubBytes applies every pattern to data.
func scrubBytes(data []byte, patterns []scrubPattern) []byte {
	for _, p := range patterns {
		data = p.re.ReplaceAll(data, []byte(p.replacement))
	}
	return data
}

// verifyClean searches data for every pattern and every known needle again.
func verifyClean(member string, data []byte, patterns []scrubPattern, needles [][]byte) []scrubFinding {
	var findings []scrubFinding
	for _, p := range patterns {
		if p.re.Match(data) {
			findings = append(findings, scrubFinding{member: member, what: "matches " + p.name + " after scrubbing"})
		}
	}
	for i, needle := range needles {
		if bytes.Contains(data, needle) {
			findings = append(findings, scrubFinding{member: member, what: fmt.Sprintf("still contains known value %d", i)})
		}
	}
	return findings
}

// redactBundle rewrites a gzipped tar, removing the known values and the
// credential shapes from every member, then checks the result. It returns the
// redacted archive and whatever the check found; a caller must not write the
// archive when findings is non-empty.
//
// Separate from redactArchive rather than extending it: that one is the exact
// substitution nvkectl's diagnostics have always had, and callers of it do
// not expect a pass to fail.
func redactBundle(data []byte, secrets []string) ([]byte, []scrubFinding, error) {
	needles := redactions(secrets)
	patterns := scrubPatterns()
	denied := deniedMembers()

	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("read the site bundle: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(zr)
	var findings []scrubFinding
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read the site bundle: %w", err)
		}
		for _, d := range denied {
			if d.MatchString(header.Name) {
				findings = append(findings, scrubFinding{member: header.Name, what: "denied member name"})
			}
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s out of the site bundle: %w", header.Name, err)
		}
		for _, needle := range needles {
			content = bytes.ReplaceAll(content, needle, []byte(redactedMarker))
		}
		content = scrubBytes(content, patterns)
		findings = append(findings, verifyClean(header.Name, content, patterns, needles)...)
		header.Size = int64(len(content))
		if err := tw.WriteHeader(header); err != nil {
			return nil, nil, fmt.Errorf("write %s to the redacted bundle: %w", header.Name, err)
		}
		if _, err := tw.Write(content); err != nil {
			return nil, nil, fmt.Errorf("write %s to the redacted bundle: %w", header.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, fmt.Errorf("close the redacted bundle: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, nil, fmt.Errorf("close the redacted bundle: %w", err)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].String() < findings[j].String() })
	return out.Bytes(), findings, nil
}

// describeFindings is the text of the note written in a bundle's place.
func describeFindings(findings []scrubFinding) string {
	lines := make([]string, 0, len(findings)+2)
	lines = append(lines, "the site bundle was not written: redaction could not be verified", "")
	for _, f := range findings {
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n") + "\n"
}
