// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nkeauth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	protocolMagic        = "NKEAuth"
	protocolVersion byte = 0
	headerSize           = len(protocolMagic) + 1 + 4

	// MaxJWTSize is the largest JWT payload accepted by protocol version 0.
	MaxJWTSize = 16 * 1024
)

// WriteJWT writes one version 0 authentication record containing jwt.
// The caller owns I/O deadlines and must discard the underlying stream after
// any error.
func WriteJWT(writer io.Writer, jwt string) error {
	if writer == nil {
		return errors.New("nkeauth: writer is required")
	}
	if jwt == "" {
		return errors.New("nkeauth: jwt must not be empty")
	}
	if !utf8.ValidString(jwt) {
		return errors.New("nkeauth: jwt must be valid utf-8")
	}
	if len(jwt) > MaxJWTSize {
		return fmt.Errorf("nkeauth: jwt is %d bytes, maximum is %d", len(jwt), MaxJWTSize)
	}

	record := make([]byte, headerSize+len(jwt))
	copy(record, protocolMagic)
	record[len(protocolMagic)] = protocolVersion
	binary.BigEndian.PutUint32(record[len(protocolMagic)+1:], uint32(len(jwt)))
	copy(record[headerSize:], jwt)

	if _, err := io.Copy(writer, bytes.NewReader(record)); err != nil {
		return &writeError{cause: err}
	}
	return nil
}

// ReadJWT reads and validates one version 0 authentication record. It consumes
// exactly the record bytes and leaves any following HTTP/2 bytes unread.
// The caller owns I/O deadlines and must discard the underlying stream after
// any error.
func ReadJWT(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("nkeauth: reader is required")
	}

	header := make([]byte, headerSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", fmt.Errorf("nkeauth: reading header: %w", err)
	}
	if string(header[:len(protocolMagic)]) != protocolMagic {
		return "", errors.New("nkeauth: invalid protocol magic")
	}
	if version := header[len(protocolMagic)]; version != protocolVersion {
		return "", fmt.Errorf("nkeauth: unsupported protocol version %d", version)
	}

	payloadSize := binary.BigEndian.Uint32(header[len(protocolMagic)+1:])
	if payloadSize == 0 {
		return "", errors.New("nkeauth: jwt must not be empty")
	}
	if payloadSize > MaxJWTSize {
		return "", fmt.Errorf("nkeauth: jwt is %d bytes, maximum is %d", payloadSize, MaxJWTSize)
	}

	payload := make([]byte, payloadSize)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", fmt.Errorf("nkeauth: reading jwt payload: %w", err)
	}
	if !utf8.Valid(payload) {
		return "", errors.New("nkeauth: jwt must be valid utf-8")
	}
	return string(payload), nil
}

type writeError struct {
	cause error
}

func (e *writeError) Error() string {
	return "nkeauth: writing authentication record"
}

func (e *writeError) Unwrap() error {
	return e.cause
}
