// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

// retryMaxAttempts and retryBaseWait tune the backoff Upload uses against
// transient GCS/auth hiccups -- a single dropped connection should not lose a
// whole dashboard's worth of evidence. Uploads are create-only, so retrying
// is safe: a retry after a prior attempt actually succeeded just finds the
// object already there and returns success.
const (
	retryMaxAttempts = 3
	retryBaseWait    = 2 * time.Second
)

// GCSUploader writes to one GCS bucket, using whatever credentials are
// already in the environment (Application Default Credentials -- the
// workflow's google-github-actions/auth step sets these up before this
// runs; nothing here handles auth itself).
type GCSUploader struct {
	Bucket string
	Client *storage.Client

	// sleep is swapped out in tests so retry backoff does not slow down
	// `go test`. Defaults to time.Sleep.
	sleep func(time.Duration)

	// doUpload is swapped out in tests so the retry loop can be exercised
	// without a real GCS client. Defaults to uploadOnce.
	doUpload func(ctx context.Context, path string, data []byte, contentType string) error
}

// NewGCSUploader opens a GCS client against Application Default Credentials.
func NewGCSUploader(ctx context.Context, bucket string) (*GCSUploader, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening GCS client: %w", err)
	}
	u := &GCSUploader{Bucket: bucket, Client: client, sleep: time.Sleep}
	u.doUpload = u.uploadOnce
	return u, nil
}

// Close releases the underlying GCS client's connections.
func (u *GCSUploader) Close() error { return u.Client.Close() }

// Upload writes data to path under Bucket, create-only: an object already at
// path is left untouched, not overwritten -- either a prior attempt of this
// same upload already succeeded (a retry after a transient failure) or this
// exact build path was already published, and in both cases not writing is
// correct. Retries on failure with exponential backoff, up to
// retryMaxAttempts, unless ctx is done -- a canceled context should fail
// fast, not keep retrying.
func (u *GCSUploader) Upload(ctx context.Context, path string, data []byte, contentType string) error {
	var lastErr error
	for attempt := 1; attempt <= retryMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = u.doUpload(ctx, path, data, contentType)
		if lastErr == nil {
			return nil
		}
		if attempt < retryMaxAttempts {
			u.sleep(retryBaseWait * time.Duration(1<<(attempt-1)))
		}
	}
	return fmt.Errorf("after %d attempts: %w", retryMaxAttempts, lastErr)
}

func (u *GCSUploader) uploadOnce(ctx context.Context, path string, data []byte, contentType string) error {
	w := u.Client.Bucket(u.Bucket).Object(path).If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		if isPreconditionFailed(err) {
			return nil
		}
		return fmt.Errorf("writing gs://%s/%s: %w", u.Bucket, path, err)
	}
	if err := w.Close(); err != nil {
		if isPreconditionFailed(err) {
			return nil
		}
		return fmt.Errorf("closing gs://%s/%s: %w", u.Bucket, path, err)
	}
	return nil
}

// isPreconditionFailed reports whether err is GCS's response to the
// DoesNotExist precondition failing -- an object already at path, which
// uploadOnce treats as success rather than an error.
func isPreconditionFailed(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed
}
