// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentialaudit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testStore struct {
	events map[string]Event
	err    error
	ctxErr error
	bound  time.Duration
}

func (s *testStore) RecordCredentialAuditEvent(ctx context.Context, event Event) error {
	s.ctxErr = ctx.Err()
	deadline, ok := ctx.Deadline()
	if ok {
		s.bound = time.Until(deadline)
	}
	if s.err != nil {
		return s.err
	}
	if s.events == nil {
		s.events = make(map[string]Event)
	}
	if _, ok := s.events[event.ID]; !ok {
		s.events[event.ID] = event
	}
	return nil
}

func TestRecorderFailureIsObservableAndDoesNotLeakErrorOrInput(t *testing.T) {
	var logs bytes.Buffer
	var observations []string
	store := &testStore{err: errors.New("sink error with example-token and private material")}
	recorder := NewRecorder(store, slog.New(slog.NewJSONHandler(&logs, nil)), func(result string) {
		observations = append(observations, result)
	})
	require.False(t, recorder.Record(t.Context(), validEvent()))
	require.Equal(t, []string{WriteFailed}, observations)
	require.Contains(t, logs.String(), WriteFailed)
	require.NotContains(t, logs.String(), "example-token")
	require.Empty(t, store.events)
	input := validEvent()
	input.Reason = "example-token"
	require.False(t, recorder.Record(t.Context(), input))
	require.Equal(t, []string{WriteFailed, WriteInvalid}, observations)
	require.NotContains(t, logs.String(), "example-token")
}

func TestRecorderRetriesDeduplicateAndOutliveCancellationWithinBound(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	store := &testStore{}
	recorder := NewRecorder(store, nil, nil)
	input := validEvent()
	require.True(t, recorder.Record(ctx, input))
	input.Timestamp = input.Timestamp.Add(time.Hour)
	require.True(t, recorder.Record(ctx, input), "a repeated ID is held by the ledger")
	require.NoError(t, store.ctxErr)
	require.Positive(t, store.bound)
	require.LessOrEqual(t, store.bound, 2*time.Second)
	require.Len(t, store.events, 1)
	for _, event := range store.events {
		require.Equal(t, input.Timestamp.Add(-time.Hour), event.Timestamp)
	}
	var absent *Recorder
	require.False(t, absent.Record(ctx, input))
}
