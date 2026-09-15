// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nkeauth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const testJWT = "header.payload.signature"

func TestUnit_NilIO(t *testing.T) {
	if err := WriteJWT(nil, testJWT); err == nil || !strings.Contains(err.Error(), "writer is required") {
		t.Fatalf("WriteJWT() error = %v, want missing writer error", err)
	}
	if _, err := ReadJWT(nil); err == nil || !strings.Contains(err.Error(), "reader is required") {
		t.Fatalf("ReadJWT() error = %v, want missing reader error", err)
	}
}

func TestUnit_WriteJWTGoldenRecord(t *testing.T) {
	var output bytes.Buffer

	if err := WriteJWT(&output, "abc"); err != nil {
		t.Fatalf("WriteJWT() error: %v", err)
	}

	want := []byte{'N', 'K', 'E', 'A', 'u', 't', 'h', 0, 0, 0, 0, 3, 'a', 'b', 'c'}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("WriteJWT() bytes = %v, want %v", output.Bytes(), want)
	}
}

func TestUnit_ReadJWTGoldenRecord(t *testing.T) {
	input := []byte{'N', 'K', 'E', 'A', 'u', 't', 'h', 0, 0, 0, 0, 3, 'a', 'b', 'c'}
	reader := iotest.OneByteReader(bytes.NewReader(input))

	got, err := ReadJWT(reader)
	if err != nil {
		t.Fatalf("ReadJWT() error: %v", err)
	}
	if got != "abc" {
		t.Fatalf("ReadJWT() = %q, want %q", got, "abc")
	}
}

func TestUnit_JWTSizeBoundary(t *testing.T) {
	maximumJWT := strings.Repeat("a", MaxJWTSize)
	var output bytes.Buffer

	if err := WriteJWT(&output, maximumJWT); err != nil {
		t.Fatalf("WriteJWT() rejected the maximum-sized JWT: %v", err)
	}
	got, err := ReadJWT(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatalf("ReadJWT() rejected the maximum-sized JWT: %v", err)
	}
	if got != maximumJWT {
		t.Fatal("maximum-sized JWT did not round trip")
	}

	tooLarge := maximumJWT + "a"
	err = WriteJWT(io.Discard, tooLarge)
	if err == nil || !strings.Contains(err.Error(), "maximum is 16384") {
		t.Fatalf("WriteJWT() oversized error = %v", err)
	}
}

func TestUnit_WriteJWTValidation(t *testing.T) {
	type parameters struct {
		jwt     string
		wantErr string
	}

	tests := map[string]parameters{
		"empty JWT": {
			wantErr: "must not be empty",
		},
		"invalid UTF-8": {
			jwt:     string([]byte{0xff}),
			wantErr: "valid utf-8",
		},
		"oversized JWT": {
			jwt:     strings.Repeat("a", MaxJWTSize+1),
			wantErr: "maximum is 16384",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			err := WriteJWT(&output, params.jwt)
			if err == nil || !strings.Contains(err.Error(), params.wantErr) {
				t.Fatalf("WriteJWT() error = %v, want error containing %q", err, params.wantErr)
			}
			if output.Len() != 0 {
				t.Fatalf("WriteJWT() wrote %d bytes for invalid input", output.Len())
			}
		})
	}
}

func TestUnit_ReadJWTRejectsMalformedRecords(t *testing.T) {
	secret := "jwt-secret-sentinel"
	type parameters struct {
		record  []byte
		wantErr string
	}

	tests := map[string]parameters{
		"short header": {
			record:  []byte("NKEAuth"),
			wantErr: "reading header",
		},
		"wrong magic": {
			record:  makeRecord("BadAuth!", 0, uint32(len(secret)), []byte(secret)),
			wantErr: "invalid protocol magic",
		},
		"unsupported version": {
			record:  makeRecord(protocolMagic, 1, uint32(len(secret)), []byte(secret)),
			wantErr: "unsupported protocol version 1",
		},
		"empty JWT": {
			record:  makeRecord(protocolMagic, 0, 0, nil),
			wantErr: "must not be empty",
		},
		"oversized declared JWT": {
			record:  makeRecord(protocolMagic, 0, MaxJWTSize+1, nil),
			wantErr: "maximum is 16384",
		},
		"truncated JWT": {
			record:  makeRecord(protocolMagic, 0, uint32(len(secret)+1), []byte(secret)),
			wantErr: "reading jwt payload",
		},
		"invalid UTF-8 JWT": {
			record:  makeRecord(protocolMagic, 0, 1, []byte{0xff}),
			wantErr: "valid utf-8",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ReadJWT(bytes.NewReader(params.record))
			if err == nil || !strings.Contains(err.Error(), params.wantErr) {
				t.Fatalf("ReadJWT() error = %v, want error containing %q", err, params.wantErr)
			}
			if got != "" {
				t.Fatalf("ReadJWT() = %q for malformed record", got)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("ReadJWT() error disclosed JWT bytes: %v", err)
			}
		})
	}
}

func TestUnit_ReadJWTPreservesHTTP2Bytes(t *testing.T) {
	preface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	input := append(makeRecord(protocolMagic, 0, uint32(len(testJWT)), []byte(testJWT)), preface...)
	reader := bytes.NewReader(input)

	got, err := ReadJWT(reader)
	if err != nil {
		t.Fatalf("ReadJWT() error: %v", err)
	}
	if got != testJWT {
		t.Fatalf("ReadJWT() = %q, want %q", got, testJWT)
	}
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading remaining bytes: %v", err)
	}
	if !bytes.Equal(remaining, preface) {
		t.Fatalf("remaining bytes = %q, want HTTP/2 preface %q", remaining, preface)
	}
}

func TestUnit_AuthenticationRecordRoundTrip(t *testing.T) {
	agent, controller := net.Pipe()
	t.Cleanup(func() {
		_ = agent.Close()
		_ = controller.Close()
	})

	writeResult := make(chan error, 1)
	go func() {
		writeResult <- WriteJWT(agent, testJWT)
	}()

	got, err := ReadJWT(controller)
	if err != nil {
		t.Fatalf("ReadJWT() error: %v", err)
	}
	if got != testJWT {
		t.Fatalf("ReadJWT() = %q, want %q", got, testJWT)
	}
	if err := <-writeResult; err != nil {
		t.Fatalf("WriteJWT() error: %v", err)
	}
}

func TestUnit_AuthenticationRecordHonorsCallerDeadline(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()

		if err := left.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
			t.Fatalf("SetReadDeadline() error: %v", err)
		}
		_, err := ReadJWT(left)
		assertTimeoutError(t, err)
	})

	t.Run("write", func(t *testing.T) {
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()

		if err := left.SetWriteDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
			t.Fatalf("SetWriteDeadline() error: %v", err)
		}
		err := WriteJWT(left, testJWT)
		assertTimeoutError(t, err)
	})
}

func TestUnit_JWTIsNotDisclosedInWriteErrors(t *testing.T) {
	secret := "jwt-secret-sentinel"
	var writerError error
	writer := writerFunc(func(record []byte) (int, error) {
		writerError = errors.New(string(record))
		return 0, writerError
	})

	err := WriteJWT(writer, secret)
	if err == nil {
		t.Fatal("WriteJWT() unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("WriteJWT() error disclosed JWT: %v", err)
	}
	if writerError == nil || !strings.Contains(writerError.Error(), secret) {
		t.Fatalf("writer error = %v, want test error containing JWT", writerError)
	}
	if !errors.Is(err, writerError) {
		t.Fatalf("WriteJWT() error = %v, want wrapped writer error", err)
	}
}

func makeRecord(magic string, version byte, declaredSize uint32, payload []byte) []byte {
	record := make([]byte, headerSize+len(payload))
	copy(record, magic)
	record[len(protocolMagic)] = version
	binary.BigEndian.PutUint32(record[len(protocolMagic)+1:], declaredSize)
	copy(record[headerSize:], payload)
	return record
}

func assertTimeoutError(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("operation unexpectedly succeeded")
	}
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("operation error = %v, want network timeout", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(data []byte) (int, error) { return f(data) }
