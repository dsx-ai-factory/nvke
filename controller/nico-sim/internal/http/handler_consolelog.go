// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"bytes"
	"compress/gzip"
	"io"
	nethttp "net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type consoleLogFile struct {
	source string
	name   string
}

// consoleLogFiles are the known console log files produced by each VM backend.
var consoleLogFiles = []consoleLogFile{
	{source: "console", name: "console.log"}, // darwin-qemu
	{source: "ttyS0", name: "disk.qcow2.ttyS0.log"},
	{source: "ttyS1", name: "disk.qcow2.ttyS1.log"},
}

// handleConsoleLog serves VM console log content.
//
// Query parameters:
//   - order=newest  — reverse line order so most recent output comes first
//   - order=oldest  — original chronological order (default)
//   - follow=true   — stream new output as it's written (like tail -f)
//   - source=ttyS0  — choose a specific console source instead of auto
//
// Supports gzip via Accept-Encoding (except in follow mode).
// The web UI handles ANSI parsing client-side.
func (s *Server) handleConsoleLog(w nethttp.ResponseWriter, r *nethttp.Request) {
	instanceID := r.PathValue("instanceId")
	if instanceID == "" {
		nethttp.Error(w, "missing instance ID", nethttp.StatusBadRequest)
		return
	}
	if _, _, err := s.assertVMBackend(r, instanceID); err != nil {
		writeBackendError(w, err)
		return
	}

	logPath := s.findConsoleLog(instanceID, r.URL.Query().Get("source"))
	if logPath == "" {
		nethttp.Error(w, "no console log found for instance", nethttp.StatusNotFound)
		return
	}

	if r.URL.Query().Get("follow") == "true" {
		s.followConsoleLog(w, r, logPath)
		return
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		s.logger.Error("failed to read console log", "instanceID", instanceID, "error", err)
		nethttp.Error(w, "failed to read console log", nethttp.StatusInternalServerError)
		return
	}

	// Truncate at ?end=N bytes (used by the UI's background backfill fetch).
	if endStr := r.URL.Query().Get("end"); endStr != "" {
		if n, err := strconv.ParseInt(endStr, 10, 64); err == nil && n >= 0 && n < int64(len(data)) {
			data = data[:n]
		}
	}

	if r.URL.Query().Get("order") == "newest" {
		data = reverseLines(data)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write(data)
		gz.Close()
	} else {
		w.Write(data)
	}
}

// followConsoleLog keeps the HTTP connection open and streams new bytes
// as they're appended to the console log file, like tail -f.
// The connection closes when the client disconnects.
func (s *Server) followConsoleLog(w nethttp.ResponseWriter, r *nethttp.Request, logPath string) {
	flusher, ok := w.(nethttp.Flusher)
	if !ok {
		nethttp.Error(w, "streaming not supported", nethttp.StatusInternalServerError)
		return
	}

	f, err := os.Open(logPath)
	if err != nil {
		nethttp.Error(w, "failed to open console log", nethttp.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// If tail=N is set, seek to the last N lines instead of sending the whole file.
	if tailStr := r.URL.Query().Get("tail"); tailStr != "" {
		if n, err := strconv.Atoi(tailStr); err == nil && n > 0 {
			seekToLastNLines(f, n)
		}
	}

	// Expose the byte offset where tailing starts so the client can
	// backfill everything before this point in a separate request.
	tailOffset, _ := f.Seek(0, io.SeekCurrent)
	w.Header().Set("X-Tail-Offset", strconv.FormatInt(tailOffset, 10))

	// Tell the client how many bytes the initial dump is, so it can
	// buffer the full initial content before rendering (no partial flicker).
	info, _ := f.Stat()
	if info != nil {
		w.Header().Set("X-Initial-Size", strconv.FormatInt(info.Size()-tailOffset, 10))
	}

	// Send existing content in one write so the client receives it as
	// few TCP segments as possible (avoids partial-render flicker).
	initial, err := io.ReadAll(f)
	if err != nil {
		return
	}
	if len(initial) > 0 {
		w.Write(initial)
		flusher.Flush()
	}

	// Tail: poll for new data until the client disconnects.
	ctx := r.Context()
	offset := tailOffset + int64(len(initial))
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}

		// Detect file truncation (virtlogd rotation).
		if info, err := os.Stat(logPath); err == nil && info.Size() < offset {
			f.Seek(0, io.SeekStart)
			offset = 0
		}

		n, err := f.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flusher.Flush()
			offset += int64(n)
		}
		if err != nil && err != io.EOF {
			return
		}
	}
}

// seekToLastNLines positions the file to start at the last N lines.
// Reads backwards from EOF in chunks to count newlines.
func seekToLastNLines(f *os.File, n int) {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return
	}

	const chunkSize = 8192
	size := info.Size()
	offset := size
	found := 0

	buf := make([]byte, chunkSize)
	for offset > 0 && found <= n {
		readSize := int64(chunkSize)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize
		f.Seek(offset, io.SeekStart)
		nr, err := f.Read(buf[:readSize])
		if err != nil || nr == 0 {
			break
		}
		for i := nr - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				found++
				if found > n {
					// Position right after this newline.
					f.Seek(offset+int64(i)+1, io.SeekStart)
					return
				}
			}
		}
	}

	// Fewer than N lines in the file — start from beginning.
	f.Seek(0, io.SeekStart)
}

// reverseLines returns data with line order reversed.
func reverseLines(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return bytes.Join(lines, []byte("\n"))
}

// findConsoleLog locates the console log file for the given instance.
func (s *Server) findConsoleLog(instanceID, source string) string {
	instanceDir := filepath.Join(s.instancesDir, instanceID)
	if source != "" && source != "auto" {
		for _, file := range consoleLogFiles {
			if file.source != source {
				continue
			}
			path := filepath.Join(instanceDir, file.name)
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				return path
			}
			return ""
		}
		return ""
	}

	var bestPath string
	var bestModTime time.Time
	for _, file := range consoleLogFiles {
		path := filepath.Join(instanceDir, file.name)
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			if bestPath == "" || info.ModTime().After(bestModTime) {
				bestPath = path
				bestModTime = info.ModTime()
			}
		}
	}
	return bestPath
}
