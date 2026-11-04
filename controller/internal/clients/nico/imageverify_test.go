// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
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
// missing verifier" is the security property the mode exists for. A
// refactor that changes any row here changes the feature, not the code.
func TestVerifyImage(t *testing.T) {
	image := &enginev1.MachineImage{ObjectMeta: metav1.ObjectMeta{Name: "nvke-node-fde-ubuntu-24.04.5-amd64"}}
	rejection := errors.New("verifier says no")

	tests := []struct {
		name         string
		mode         string
		verifier     *stubVerifier
		wantErr      bool
		wantSentinel bool
		wantCalls    int
	}{
		{name: "off never consults the verifier", mode: ImageVerificationOff, verifier: &stubVerifier{err: rejection}, wantCalls: 0},
		{name: "empty mode is off", mode: "", verifier: &stubVerifier{err: rejection}, wantCalls: 0},
		{name: "warn with no verifier proceeds", mode: ImageVerificationWarn, verifier: nil},
		{name: "warn with a rejected image proceeds", mode: ImageVerificationWarn, verifier: &stubVerifier{err: rejection}, wantCalls: 1},
		{name: "warn with a verified image proceeds", mode: ImageVerificationWarn, verifier: &stubVerifier{}, wantCalls: 1},
		{name: "enforce with a verified image proceeds", mode: ImageVerificationEnforce, verifier: &stubVerifier{}, wantCalls: 1},
		{name: "enforce with a rejected image refuses", mode: ImageVerificationEnforce, verifier: &stubVerifier{err: rejection}, wantErr: true, wantSentinel: true, wantCalls: 1},
		{name: "enforce with no verifier fails closed", mode: ImageVerificationEnforce, verifier: nil, wantErr: true, wantSentinel: true},
		{name: "an unknown mode is an error, not a guess", mode: "audit", verifier: &stubVerifier{}, wantErr: true, wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var verifier ImageVerifier
			if tt.verifier != nil {
				verifier = tt.verifier
			}
			err := verifyImage(context.Background(), tt.mode, verifier, image, "node-test-id", "cluster-test-id")
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

// TestVerifyImageWrapsTheVerifierError pins that an enforce refusal preserves
// the verifier's own error for errors.Is, so callers can distinguish rejection
// classes without parsing strings.
func TestVerifyImageWrapsTheVerifierError(t *testing.T) {
	image := &enginev1.MachineImage{ObjectMeta: metav1.ObjectMeta{Name: "img"}}
	verifierErr := errors.New("checksum mismatch")
	err := verifyImage(context.Background(), ImageVerificationEnforce, &stubVerifier{err: verifierErr}, image, "node-test-id", "cluster-test-id")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrImageVerificationFailed)
	require.ErrorIs(t, err, verifierErr)
}

// TestEnforceDistinguishesRejectionFromOutage pins the retry contract of the
// seam: a definitive rejection (and a nil verifier — static configuration) is
// permanent; an operational verifier failure still refuses this attempt
// fail-closed but stays retryable.
func TestEnforceDistinguishesRejectionFromOutage(t *testing.T) {
	image := &enginev1.MachineImage{ObjectMeta: metav1.ObjectMeta{Name: "img-1"}}

	rejection := fmt.Errorf("untrusted signer: %w", ErrImageRejected)
	err := verifyImage(context.Background(), ImageVerificationEnforce, &stubVerifier{err: rejection}, image, "node-test-id", "cluster-test-id")
	require.ErrorIs(t, err, ErrImageVerificationFailed)
	_, terminal := shared.AsTerminal(err)
	assert.True(t, terminal, "a definitive rejection must be permanent")

	err = verifyImage(context.Background(), ImageVerificationEnforce, nil, image, "node-test-id", "cluster-test-id")
	require.ErrorIs(t, err, ErrImageVerificationFailed)
	_, terminal = shared.AsTerminal(err)
	assert.True(t, terminal, "a nil verifier is static configuration; retrying cannot help")

	outage := errors.New("registry: connection refused")
	err = verifyImage(context.Background(), ImageVerificationEnforce, &stubVerifier{err: outage}, image, "node-test-id", "cluster-test-id")
	require.ErrorIs(t, err, ErrImageVerificationFailed, "an outage still refuses fail-closed")
	_, terminal = shared.AsTerminal(err)
	assert.False(t, terminal, "an operational failure must stay retryable")
}

type slowVerifier struct{}

func (slowVerifier) VerifyImage(ctx context.Context, _ *enginev1.MachineImage) error {
	<-ctx.Done()
	return ctx.Err()
}

type recordingVerifier struct {
	deadline    time.Time
	hasDeadline bool
}

func (r *recordingVerifier) VerifyImage(ctx context.Context, _ *enginev1.MachineImage) error {
	r.deadline, r.hasDeadline = ctx.Deadline()
	return nil
}

func TestVerifyImageBoundsTheVerifier(t *testing.T) {
	image := &enginev1.MachineImage{ObjectMeta: metav1.ObjectMeta{Name: "img"}}

	t.Run("each call gets the pre-flight deadline", func(t *testing.T) {
		verifier := &recordingVerifier{}
		start := time.Now()
		require.NoError(t, verifyImage(context.Background(), ImageVerificationEnforce, verifier, image, "node-test-id", "cluster-test-id"))
		require.True(t, verifier.hasDeadline, "the verifier must be called with a deadline")
		assert.WithinDuration(t, start.Add(imageVerificationTimeout), verifier.deadline, time.Second)
	})

	t.Run("running out of time names the deadline and stays retryable", func(t *testing.T) {
		err := runVerifier(context.Background(), slowVerifier{}, image, 10*time.Millisecond)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "did not finish within 10ms")
		require.NotErrorIs(t, err, ErrImageRejected, "a timeout is not a rejection")
	})

	t.Run("a caller deadline is reported as the caller's", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		err := runVerifier(ctx, slowVerifier{}, image, time.Minute)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotContains(t, err.Error(), "did not finish within", "the pre-flight's own deadline did not end this call")
	})

	t.Run("warn proceeds when the verifier runs out of time", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		require.NoError(t, verifyImage(ctx, ImageVerificationWarn, slowVerifier{}, image, "node-test-id", "cluster-test-id"))
	})

	t.Run("enforce refuses a timed-out attempt but keeps it retryable", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		err := verifyImage(ctx, ImageVerificationEnforce, slowVerifier{}, image, "node-test-id", "cluster-test-id")
		require.ErrorIs(t, err, ErrImageVerificationFailed)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		_, terminal := shared.AsTerminal(err)
		assert.False(t, terminal, "a timeout must stay retryable")
	})
}
