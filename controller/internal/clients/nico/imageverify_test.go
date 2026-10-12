// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// stubVerifier counts calls and returns a fixed error.
type stubVerifier struct {
	calls int
	err   error
}

func (s *stubVerifier) VerifyImage(context.Context, *enginev1.MachineImage) error {
	s.calls++
	return s.err
}

// TestVerifyImage pins the pre-flight's mode semantics. These are behavioral
// guarantees, not implementation details: "off is a true no-op" is what makes
// the default configuration safe to merge, and "enforce fails closed on a
// missing verdict source" is the security property the mode exists for. A
// refactor that changes any row here changes the feature, not the code.
func TestVerifyImage(t *testing.T) {
	image := &enginev1.MachineImage{ObjectMeta: metav1.ObjectMeta{Name: "nke-node-fde-ubuntu-24.04.5-amd64"}}
	verdictErr := errors.New("verdict says no")

	tests := []struct {
		name         string
		mode         string
		verifier     *stubVerifier
		wantErr      bool
		wantSentinel bool
		wantCalls    int
	}{
		{name: "off never consults the verifier", mode: ImageVerificationOff, verifier: &stubVerifier{err: verdictErr}, wantCalls: 0},
		{name: "empty mode is off", mode: "", verifier: &stubVerifier{err: verdictErr}, wantCalls: 0},
		{name: "warn with no verdict source proceeds", mode: ImageVerificationWarn, verifier: nil},
		{name: "warn with a failing verdict proceeds", mode: ImageVerificationWarn, verifier: &stubVerifier{err: verdictErr}, wantCalls: 1},
		{name: "warn with a passing verdict proceeds", mode: ImageVerificationWarn, verifier: &stubVerifier{}, wantCalls: 1},
		{name: "enforce with a passing verdict proceeds", mode: ImageVerificationEnforce, verifier: &stubVerifier{}, wantCalls: 1},
		{name: "enforce with a failing verdict refuses", mode: ImageVerificationEnforce, verifier: &stubVerifier{err: verdictErr}, wantErr: true, wantSentinel: true, wantCalls: 1},
		{name: "enforce with no verdict source fails closed", mode: ImageVerificationEnforce, verifier: nil, wantErr: true, wantSentinel: true},
		{name: "an unknown mode is an error, not a guess", mode: "audit", verifier: &stubVerifier{}, wantErr: true, wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var verifier ImageVerifier
			if tt.verifier != nil {
				verifier = tt.verifier
			}
			err := verifyImage(context.Background(), tt.mode, verifier, image)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.wantSentinel {
				require.ErrorIs(t, err, ErrImageVerificationFailed)
				require.ErrorContains(t, err, image.Name, "a refusal names the image it refused")
			} else {
				require.NotErrorIs(t, err, ErrImageVerificationFailed)
			}
			if tt.verifier != nil {
				assert.Equal(t, tt.wantCalls, tt.verifier.calls)
			}
		})
	}
}

// TestVerifyImageWrapsTheVerdict pins that an enforce refusal preserves the
// verifier's own error for errors.Is, so callers can distinguish verdict
// classes without parsing strings.
func TestVerifyImageWrapsTheVerdict(t *testing.T) {
	image := &enginev1.MachineImage{ObjectMeta: metav1.ObjectMeta{Name: "img"}}
	verdictErr := errors.New("checksum mismatch")
	err := verifyImage(context.Background(), ImageVerificationEnforce, &stubVerifier{err: verdictErr}, image)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrImageVerificationFailed)
	require.ErrorIs(t, err, verdictErr)
}
