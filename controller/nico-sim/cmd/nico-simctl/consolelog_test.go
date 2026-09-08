// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// chunkReader yields each chunk from one Read call, then returns
// errStopFollow to terminate followStream.
type chunkReader struct {
	chunks [][]byte
	i      int
}

var errStopFollow = errors.New("stop")

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, errStopFollow
	}
	c := r.chunks[r.i]
	r.i++
	n := copy(p, c)
	return n, nil
}

func runFollow(t *testing.T, chunks [][]byte) string {
	t.Helper()
	var out bytes.Buffer
	err := followStream(&chunkReader{chunks: chunks}, &out, 0)
	if !errors.Is(err, errStopFollow) {
		t.Fatalf("followStream returned unexpected error: %v", err)
	}
	return out.String()
}

func TestFollowStream_EscapeSequenceSplitAcrossReads(t *testing.T) {
	const tail16 = "0123456789abcdef"
	tests := []struct {
		name   string
		chunks [][]byte
		want   string
	}{
		{
			name:   "complete sequence in single chunk is stripped",
			chunks: [][]byte{[]byte("hello\x1b[2Jworld")},
			want:   "helloworld",
		},
		{
			name: "ESC split from CSI body — both chunks cleaned",
			chunks: [][]byte{
				[]byte("hello\x1b"),
				[]byte("[2Jworld"),
			},
			want: "helloworld",
		},
		{
			name: "ESC + partial CSI split from terminator",
			chunks: [][]byte{
				[]byte("hello\x1b[2"),
				[]byte("Jworld"),
			},
			want: "helloworld",
		},
		{
			name: "color SGR is preserved across split",
			chunks: [][]byte{
				[]byte("pre\x1b"),
				[]byte("[32mgreen\x1b[0m"),
			},
			want: "pre\x1b[32mgreen\x1b[0m",
		},
		{
			name:   "lone trailing ESC beyond tail cap is flushed",
			chunks: [][]byte{[]byte("\x1b" + tail16)},
			want:   "\x1b" + tail16,
		},
		{
			name: "CRLF normalized across chunk boundary",
			chunks: [][]byte{
				[]byte("line1\r"),
				[]byte("\nline2"),
			},
			want: "line1\nline2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runFollow(t, tc.chunks); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFollowStream_FlushesPendingOnTerminalError(t *testing.T) {
	// A lone ESC with no following bytes should be flushed when the
	// stream terminates with a non-EOF error.
	var out bytes.Buffer
	err := followStream(&chunkReader{chunks: [][]byte{[]byte("abc\x1b")}}, &out, 0)
	if !errors.Is(err, errStopFollow) {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := out.String(), "abc\x1b"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// Sanity-check: an io.EOF from the reader is treated as "wait and retry",
// not terminal. We wrap a chunkReader that returns io.EOF once then the
// stop sentinel to verify EOF doesn't exit the loop.
type eofThenStopReader struct {
	eofSent bool
}

func (r *eofThenStopReader) Read(p []byte) (int, error) {
	if !r.eofSent {
		r.eofSent = true
		return 0, io.EOF
	}
	return 0, errStopFollow
}

func TestFollowStream_EOFIsNotTerminal(t *testing.T) {
	var out bytes.Buffer
	err := followStream(&eofThenStopReader{}, &out, 0)
	if !errors.Is(err, errStopFollow) {
		t.Fatalf("unexpected error: %v", err)
	}
}
