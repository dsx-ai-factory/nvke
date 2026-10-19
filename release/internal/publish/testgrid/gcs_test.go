// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func noSleepUploader(doUpload func(ctx context.Context, path string, data []byte, contentType string) error) *GCSUploader {
	return &GCSUploader{
		Bucket:   "test-bucket",
		sleep:    func(context.Context, time.Duration) error { return nil },
		doUpload: doUpload,
	}
}

// retryableErr is a googleapi.Error with a 5xx code, the kind
// storage.ShouldRetry recognizes -- a plain errors.New is not retryable, so
// it cannot stand in for a transient failure in these tests.
func retryableErr(msg string) error {
	return &googleapi.Error{Code: http.StatusServiceUnavailable, Message: msg}
}

func TestUploadSucceedsFirstTry(t *testing.T) {
	calls := 0
	u := noSleepUploader(func(context.Context, string, []byte, string) error {
		calls++
		return nil
	})

	if err := u.Upload(context.Background(), "path", nil, "application/json"); err != nil {
		t.Fatalf("Upload returned %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("doUpload called %d times, want 1", calls)
	}
}

func TestUploadRetriesOnFailureThenSucceeds(t *testing.T) {
	calls := 0
	errBoom := retryableErr("transient connect timeout")
	u := noSleepUploader(func(context.Context, string, []byte, string) error {
		calls++
		if calls < 3 {
			return errBoom
		}
		return nil
	})

	if err := u.Upload(context.Background(), "path", nil, "application/json"); err != nil {
		t.Fatalf("Upload returned %v, want nil", err)
	}
	if calls != 3 {
		t.Errorf("doUpload called %d times, want 3", calls)
	}
}

func TestUploadGivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	errBoom := retryableErr("connect timeout")
	u := noSleepUploader(func(context.Context, string, []byte, string) error {
		calls++
		return errBoom
	})

	err := u.Upload(context.Background(), "path", nil, "application/json")
	if err == nil {
		t.Fatal("Upload returned nil, want an error")
	}
	if calls != retryMaxAttempts {
		t.Errorf("doUpload called %d times, want %d", calls, retryMaxAttempts)
	}
	if !errors.Is(err, errBoom) {
		t.Errorf("error %v does not wrap the last underlying error", err)
	}
}

// A non-retryable error (permission denied, not found) will not start
// succeeding on a later attempt, so Upload must not spend the retry budget
// on it.
func TestUploadDoesNotRetryNonRetryableError(t *testing.T) {
	calls := 0
	errPermanent := &googleapi.Error{Code: http.StatusForbidden, Message: "permission denied"}
	u := noSleepUploader(func(context.Context, string, []byte, string) error {
		calls++
		return errPermanent
	})

	err := u.Upload(context.Background(), "path", nil, "application/json")
	if err == nil {
		t.Fatal("Upload returned nil, want an error")
	}
	if calls != 1 {
		t.Errorf("doUpload called %d times, want 1 -- a non-retryable error must not be retried", calls)
	}
	if !errors.Is(err, errPermanent) {
		t.Errorf("error %v does not wrap the underlying error", err)
	}
}

func TestUploadStopsRetryingOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	u := noSleepUploader(func(context.Context, string, []byte, string) error {
		calls++
		return errors.New("should not be reached")
	})

	if err := u.Upload(ctx, "path", nil, "application/json"); err == nil {
		t.Fatal("Upload returned nil, want context.Canceled")
	}
	if calls != 0 {
		t.Errorf("doUpload called %d times, want 0 for an already-canceled context", calls)
	}
}

// A context canceled mid-backoff must interrupt the sleep itself, not wait
// out the full backoff before the next ctx.Err() check at the top of the
// loop.
func TestUploadStopsMidBackoffOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	u := &GCSUploader{
		Bucket: "test-bucket",
		sleep: func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctxSleep(ctx, d)
		},
		doUpload: func(context.Context, string, []byte, string) error {
			calls++
			return retryableErr("transient")
		},
	}

	err := u.Upload(ctx, "path", nil, "application/json")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("doUpload called %d times, want 1 -- canceling during the first backoff must stop before a second attempt", calls)
	}
}

func TestCtxSleepReturnsEarlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if err := ctxSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ctxSleep took %v, want it to return immediately for an already-canceled context", elapsed)
	}
}

func TestIsPreconditionFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"412 precondition failed", &googleapi.Error{Code: http.StatusPreconditionFailed}, true},
		{
			"wrapped 412",
			fmt.Errorf("closing gs://bucket/path: %w", &googleapi.Error{Code: http.StatusPreconditionFailed}),
			true,
		},
		{"a different googleapi error code", &googleapi.Error{Code: http.StatusInternalServerError}, false},
		{"a non-googleapi error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPreconditionFailed(tc.err); got != tc.want {
				t.Errorf("isPreconditionFailed(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
