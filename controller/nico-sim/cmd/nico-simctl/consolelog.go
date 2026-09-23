// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// stripCursorSequences matches ANSI escape sequences that manipulate cursor
// position, erase screen/lines, change terminal modes, or scroll. SGR
// sequences (ESC[...m) for colors and styles are intentionally NOT matched.
var stripCursorSequences = regexp.MustCompile(
	// CSI sequences (ESC[ ... <letter>) for cursor/erase/scroll/mode:
	`\x1b\[\?[0-9;]*[hl]` + // DECSET/DECRST: ESC[?<n>h / ESC[?<n>l
		`|` + `\x1b\[[0-9;]*[ABCDEFGHJKSTfdsu@PLMRrg]` + // cursor move, erase, scroll, insert/delete, save/restore, region, tabs
		// Two-char ESC sequences:
		`|` + `\x1b[DM78]` + // index, reverse index, save/restore cursor (DEC)
		`|` + `\x1b c` + // full terminal reset (RIS)
		`|` + `\x1b\[!p` + // soft terminal reset
		`|` + `\x1b H`, // tab set
)

// cleanConsoleOutput strips cursor-control escape sequences and bare
// carriage returns from raw VM console output while preserving colors.
func cleanConsoleOutput(s string) string {
	s = stripCursorSequences.ReplaceAllString(s, "")
	// Strip bare \r that isn't part of \r\n. Protect \r\n first.
	s = strings.ReplaceAll(s, "\r\n", "\n") // normalize CRLF to LF
	s = strings.ReplaceAll(s, "\r", "")     // strip bare CR
	return s
}

// maxEscapeTail is the largest trailing tail we retain across reads while
// waiting for an escape sequence terminator. Beyond this, we flush the
// fragment so a stray lone ESC byte doesn't stall output indefinitely.
const maxEscapeTail = 16

// vmConsoleLog represents a discovered console log file for a VM.
type vmConsoleLog struct {
	InstanceID string
	Name       string // local-hostname from meta-data file, empty if unavailable
	LogFile    string
	ParentDir  string // the nico-sim working directory (e.g. /tmp/nico-vms-integration/...)
}

// readInstanceName reads the local-hostname from the meta-data file in an
// instance directory. Returns empty string if the file is missing or unparseable.
func readInstanceName(instanceDir string) string {
	data, err := os.ReadFile(filepath.Join(instanceDir, "meta-data"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if after, ok := strings.CutPrefix(line, "local-hostname:"); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// runConsoleLog implements the "instance console-log" subcommand.
// It queries the nico-sim API to find a VM by name or ID, reads its
// SerialConsoleUrl, and streams the console log over SSH.
func runConsoleLog(args []string) {
	opts := parseCommon(args)
	siteID := optionalStringArg(args, "--site-id")
	name := optionalStringArg(args, "--name")
	id := optionalStringArg(args, "--id")
	dump := hasFlag(args, "--dump")
	follow := !dump

	if name != "" && id != "" {
		fmt.Fprintln(os.Stderr, "--name and --id are mutually exclusive")
		os.Exit(1)
	}
	query := name
	if id != "" {
		query = id
	}
	if query == "" {
		fmt.Fprintln(os.Stderr, "--name or --id is required")
		os.Exit(1)
	}

	runRemoteConsoleLog(opts.BaseURL, query, opts.Org, siteID, opts.Token, follow)
}

// runConsoleLogLocal implements the "instance console-log-local" subcommand.
// It discovers VM console logs on disk, auto-selects when unambiguous,
// and streams the log content preserving ANSI escape sequences.
func runConsoleLogLocal(args []string) {
	dump := hasFlag(args, "--dump")
	follow := !dump
	lines := optionalIntArg(args, "--lines", 0)
	name := optionalStringArg(args, "--name")
	id := optionalStringArg(args, "--id")
	dir := optionalStringArg(args, "--dir")

	if name != "" && id != "" {
		fmt.Fprintln(os.Stderr, "--name and --id are mutually exclusive")
		os.Exit(1)
	}

	logs := discoverConsoleLogs(dir)
	if len(logs) == 0 {
		fmt.Fprintln(os.Stderr, "No console logs found.")
		fmt.Fprintln(os.Stderr, "Searched:")
		for _, d := range searchDirs("") {
			fmt.Fprintf(os.Stderr, "  %s\n", d)
		}
		os.Exit(1)
	}

	selected := logs
	switch {
	case id != "":
		selected = filterByID(logs, id)
		if len(selected) == 0 {
			fmt.Fprintf(os.Stderr, "No VM matching id %q found.\nAvailable VMs:\n", id)
			printAvailableVMs(logs)
			os.Exit(1)
		}
	case name != "":
		selected = filterByName(logs, name)
		if len(selected) == 0 {
			fmt.Fprintf(os.Stderr, "No VM matching name %q found.\nAvailable VMs:\n", name)
			printAvailableVMs(logs)
			os.Exit(1)
		}
	}

	if len(selected) > 1 {
		fmt.Fprintln(os.Stderr, "Multiple VMs found. Pass --name or --id to select one:")
		printAvailableVMs(selected)
		os.Exit(1)
	}

	vm := selected[0]
	info, err := os.Stat(vm.LogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot read log: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Console log: %s (%d bytes)\n---\n", vm.LogFile, info.Size())

	if follow {
		tailFollow(vm.LogFile, lines)
	} else if lines > 0 {
		tailLines(vm.LogFile, lines)
	} else {
		catFile(vm.LogFile)
	}
}

// searchDirs returns all directories that may contain nico-vms instance directories.
func searchDirs(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}

	var dirs []string

	// Default server location: ./var/instances
	if abs, err := filepath.Abs("./var/instances"); err == nil {
		dirs = append(dirs, abs)
	}

	// Search both os.TempDir() (per-user sandbox on macOS) and /tmp
	// to cover all possible locations.
	tmpRoots := uniquePaths(os.TempDir(), "/tmp")

	for _, tmpDir := range tmpRoots {
		// Integration test directories: {tmp}/nico-vms-integration/*/instances
		integrationRoot := filepath.Join(tmpDir, "nico-vms-integration")
		if entries, err := os.ReadDir(integrationRoot); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					dirs = append(dirs, filepath.Join(integrationRoot, e.Name(), "instances"))
				}
			}
		}

		// Test-server directories: {tmp}/nico-sim-test-*/instances
		matches, _ := filepath.Glob(filepath.Join(tmpDir, "nico-sim-test-*"))
		for _, m := range matches {
			dirs = append(dirs, filepath.Join(m, "instances"))
		}
	}

	return dirs
}

// consoleLogNames are the known console log file names produced by each backend.
var consoleLogNames = []string{
	"console.log",          // darwin-qemu
	"disk.qcow2.ttyS0.log", // libvirt serial 0
	"disk.qcow2.ttyS1.log", // libvirt serial 1
}

// discoverConsoleLogs walks all search directories and returns every
// console log found, grouped by instance.
func discoverConsoleLogs(explicit string) []vmConsoleLog {
	var results []vmConsoleLog
	seen := map[string]bool{}

	for _, dir := range searchDirs(explicit) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			instanceID := entry.Name()
			instanceDir := filepath.Join(dir, instanceID)
			logPath := newestConsoleLog(instanceDir)
			if logPath == "" {
				continue
			}
			if seen[logPath] {
				continue
			}
			seen[logPath] = true
			results = append(results, vmConsoleLog{
				InstanceID: instanceID,
				Name:       readInstanceName(instanceDir),
				LogFile:    logPath,
				ParentDir:  dir,
			})
		}
	}

	// Sort by modification time (newest first) for deterministic selection.
	sort.Slice(results, func(i, j int) bool {
		ti := modTime(results[i].LogFile)
		tj := modTime(results[j].LogFile)
		return ti.After(tj)
	})

	return results
}

func newestConsoleLog(instanceDir string) string {
	var bestPath string
	var bestModTime time.Time
	for _, logName := range consoleLogNames {
		logPath := filepath.Join(instanceDir, logName)
		if info, err := os.Stat(logPath); err == nil && info.Size() > 0 {
			if bestPath == "" || info.ModTime().After(bestModTime) {
				bestPath = logPath
				bestModTime = info.ModTime()
			}
		}
	}
	return bestPath
}

func modTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// filterByName returns logs whose Name (local-hostname from meta-data) matches
// the given string. Exact match is preferred; falls back to substring match.
func filterByName(logs []vmConsoleLog, name string) []vmConsoleLog {
	// Exact match first.
	for _, l := range logs {
		if l.Name == name {
			return []vmConsoleLog{l}
		}
	}
	// Substring match.
	var matched []vmConsoleLog
	lower := strings.ToLower(name)
	for _, l := range logs {
		if strings.Contains(strings.ToLower(l.Name), lower) {
			matched = append(matched, l)
		}
	}
	return matched
}

// filterByID returns logs whose InstanceID (UUID directory name) matches
// the given string. Exact match is preferred; falls back to substring match.
func filterByID(logs []vmConsoleLog, id string) []vmConsoleLog {
	// Exact match first.
	for _, l := range logs {
		if l.InstanceID == id {
			return []vmConsoleLog{l}
		}
	}
	// Substring match.
	var matched []vmConsoleLog
	lower := strings.ToLower(id)
	for _, l := range logs {
		if strings.Contains(strings.ToLower(l.InstanceID), lower) {
			matched = append(matched, l)
		}
	}
	return matched
}

func printAvailableVMs(logs []vmConsoleLog) {
	for _, l := range logs {
		info, _ := os.Stat(l.LogFile)
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		name := l.Name
		if name == "" {
			name = "(unknown)"
		}
		fmt.Fprintf(os.Stderr, "  %-40s %-45s %s (%d bytes)\n", l.InstanceID, name, l.ParentDir, size)
	}
}

// catFile writes the entire file to stdout with cursor sequences stripped.
func catFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.WriteString(cleanConsoleOutput(string(data)))
}

// tailLines prints the last n lines of a file to stdout.
func tailLines(path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}

	cleaned := cleanConsoleOutput(string(data))
	lines := strings.Split(cleaned, "\n")
	// Remove trailing empty line from split.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		fmt.Println(line)
	}
}

// tailFollow streams the file to stdout, waiting for new data like tail -f.
// If n > 0, it prints the last n lines before following.
func tailFollow(path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	if n > 0 {
		// Read the whole file, print last n lines, then continue from end.
		data, err := io.ReadAll(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read: %v\n", err)
			os.Exit(1)
		}
		cleaned := cleanConsoleOutput(string(data))
		lines := strings.Split(cleaned, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		start := len(lines) - n
		if start < 0 {
			start = 0
		}
		for _, line := range lines[start:] {
			fmt.Println(line)
		}
		// f is now at EOF; fall through to follow loop.
	} else {
		// Print entire file first, then follow.
		data, err := io.ReadAll(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.WriteString(cleanConsoleOutput(string(data)))
	}

	// followStream only returns once reading has failed for good (it loops on
	// io.EOF), so its return value is always a real error.
	err = followStream(f, os.Stdout, 200*time.Millisecond)
	fmt.Fprintf(os.Stderr, "\nread error: %v\n", err)
	os.Exit(1)
}

// followStream reads from r and writes the cleaned output to w, tail -f
// style: io.EOF is treated as "no data yet — wait and retry" after
// sleeping pollInterval. Any other error terminates the loop and is
// returned after flushing any retained tail. Chunks are buffered so an
// escape sequence split across reads is handled correctly.
func followStream(r io.Reader, w io.Writer, pollInterval time.Duration) error {
	buf := make([]byte, 4096)
	var pending []byte
	for {
		nr, err := r.Read(buf)
		if nr > 0 {
			pending = append(pending, buf[:nr]...)
			flushEnd := escapeSafeSplit(pending)
			if flushEnd > 0 {
				if _, werr := io.WriteString(w, cleanConsoleOutput(string(pending[:flushEnd]))); werr != nil {
					return werr
				}
				pending = pending[flushEnd:]
			}
		}
		if err == io.EOF {
			time.Sleep(pollInterval)
			continue
		}
		if err != nil {
			if len(pending) > 0 {
				_, _ = io.WriteString(w, cleanConsoleOutput(string(pending)))
			}
			return err
		}
	}
}

// escapeSafeSplit returns the index in pending up to which bytes can be
// flushed to cleanConsoleOutput without risking a split escape sequence.
// If the tail starts with an ESC (0x1b) within the last maxEscapeTail
// bytes, that suffix is retained for the next read.
func escapeSafeSplit(pending []byte) int {
	flushEnd := len(pending)
	if i := bytes.LastIndexByte(pending, 0x1b); i >= 0 && len(pending)-i < maxEscapeTail {
		flushEnd = i
	}
	return flushEnd
}

// runRemoteConsoleLog fetches the console log for a VM by calling the
// nico-sim API to discover the SerialConsoleUrl and then connecting via SSH.
func runRemoteConsoleLog(serverURL, name, org, site, token string, follow bool) {
	if name == "" {
		fmt.Fprintln(os.Stderr, "--name is required")
		os.Exit(1)
	}

	endpoint := fmt.Sprintf("%s/v2/org/%s/forge/instance?pageSize=100&pageNumber=1&orderBy=CREATED_ASC",
		serverURL, org)
	if site != "" {
		endpoint += "&siteId=" + site
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "API request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "API returned status %d: %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	var instances []struct {
		ID               string  `json:"id"`
		Name             string  `json:"name"`
		SerialConsoleUrl *string `json:"serialConsoleUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&instances); err != nil {
		fmt.Fprintf(os.Stderr, "decode response: %v\n", err)
		os.Exit(1)
	}

	// Find matching instance (exact match first, then substring).
	type instanceMatch struct {
		ID               string
		Name             string
		SerialConsoleUrl *string
	}
	var match *instanceMatch
	for i, inst := range instances {
		if inst.ID == name || inst.Name == name {
			match = &instanceMatch{instances[i].ID, instances[i].Name, instances[i].SerialConsoleUrl}
			break
		}
	}
	if match == nil {
		lower := strings.ToLower(name)
		var candidates []instanceMatch
		for _, inst := range instances {
			if strings.Contains(strings.ToLower(inst.ID), lower) || strings.Contains(strings.ToLower(inst.Name), lower) {
				candidates = append(candidates, instanceMatch{inst.ID, inst.Name, inst.SerialConsoleUrl})
			}
		}
		if len(candidates) == 1 {
			match = &candidates[0]
		} else if len(candidates) > 1 {
			fmt.Fprintf(os.Stderr, "Multiple instances match %q:\n", name)
			for _, c := range candidates {
				fmt.Fprintf(os.Stderr, "  %-40s %s\n", c.ID, c.Name)
			}
			os.Exit(1)
		}
	}
	if match == nil {
		fmt.Fprintf(os.Stderr, "No instance matching %q found.\nAvailable instances:\n", name)
		for _, inst := range instances {
			fmt.Fprintf(os.Stderr, "  %-40s %s\n", inst.ID, inst.Name)
		}
		os.Exit(1)
	}

	if match.SerialConsoleUrl == nil || *match.SerialConsoleUrl == "" {
		fmt.Fprintf(os.Stderr, "Instance %s (%s) has no serial console URL.\n", match.ID, match.Name)
		os.Exit(1)
	}

	consoleURL := *match.SerialConsoleUrl
	fmt.Fprintf(os.Stderr, "Console: %s (%s)\nSSH URL: %s\n---\n", match.Name, match.ID, consoleURL)

	u, err := url.Parse(consoleURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid console URL %q: %v\n", consoleURL, err)
		os.Exit(1)
	}

	sshUser := u.User.Username()
	sshAddr := u.Host

	sshConfig := &ssh.ClientConfig{
		User:            sshUser,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // nico-sim dev tool
		Timeout:         10 * time.Second,
	}

	client, err := ssh.Dial("tcp", sshAddr, sshConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSH connection to %s failed: %v\n", sshAddr, err)
		os.Exit(1)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSH session failed: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()

	if !follow {
		// One-shot dump mode: send "dump" command, read all output, exit.
		output, err := session.CombinedOutput("dump")
		if err != nil {
			fmt.Fprintf(os.Stderr, "SSH command failed: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.WriteString(cleanConsoleOutput(string(output)))
		return
	}

	// Follow mode (default): stream stdout continuously.
	stdout, err := session.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSH stdout pipe failed: %v\n", err)
		os.Exit(1)
	}
	if err := session.Start(""); err != nil {
		fmt.Fprintf(os.Stderr, "SSH start failed: %v\n", err)
		os.Exit(1)
	}

	buf := make([]byte, 4096)
	for {
		nr, err := stdout.Read(buf)
		if nr > 0 {
			os.Stdout.WriteString(cleanConsoleOutput(string(buf[:nr])))
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "\nSSH read error: %v\n", err)
			}
			break
		}
	}
	_ = session.Wait()
}

// uniquePaths returns deduplicated paths after resolving symlinks.
func uniquePaths(paths ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			resolved = p
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	return out
}
