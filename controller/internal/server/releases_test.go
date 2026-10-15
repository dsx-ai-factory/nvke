// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

func TestListReleasesListsClusterReleasesAndSkipsDeprecated(t *testing.T) {
	t.Parallel()

	svc := NewReleaseService(slog.Default(), releaseClient(t,
		testClusterRelease("nvke-b", false),
		testClusterRelease("nvke-a", false),
		testClusterRelease("nvke-old", true),
		testNodePoolRelease("nvke-worker-only", false),
	))

	resp, err := svc.ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.NoError(t, err)
	ok, isOK := resp.(api.ListReleases200JSONResponse)
	require.True(t, isOK)
	assert.Equal(t, []api.Release{{Name: "nvke-a"}, {Name: "nvke-b"}}, ok.Releases)
}

func TestListReleasesNoReleasesReturnsEmptyList(t *testing.T) {
	t.Parallel()

	resp, err := NewReleaseService(slog.Default(), releaseClient(t)).ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.NoError(t, err)
	ok, isOK := resp.(api.ListReleases200JSONResponse)
	require.True(t, isOK)
	assert.NotNil(t, ok.Releases)
	assert.Empty(t, ok.Releases)
}

func TestListReleasesFailsWithoutReaderOrOnListError(t *testing.T) {
	t.Parallel()

	_, err := NewReleaseService(slog.Default(), nil).ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.ErrorContains(t, err, "release reader is not configured")

	_, err = NewReleaseService(slog.Default(), failingListClient(t, errors.New("boom"))).ListReleases(t.Context(), api.ListReleasesRequestObject{})
	require.ErrorContains(t, err, "failed to list ClusterReleases: boom")
}

func failingListClient(t *testing.T, err error) ctrlclient.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(releaseScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
			return err
		},
	}).Build()
}
