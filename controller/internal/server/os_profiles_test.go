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

	"github.com/dsx-ai-factory/nke/controller/api"
)

func TestListOSProfilesListsClusterReleasesAndSkipsDeprecated(t *testing.T) {
	t.Parallel()

	svc := NewOSProfileService(slog.Default(), releaseClient(t,
		testClusterRelease("nke-b", false),
		testClusterRelease("nke-a", false),
		testClusterRelease("nke-old", true),
		testNodePoolRelease("nke-worker-only", false),
	))

	resp, err := svc.ListOSProfiles(t.Context(), api.ListOSProfilesRequestObject{})
	require.NoError(t, err)
	ok, isOK := resp.(api.ListOSProfiles200JSONResponse)
	require.True(t, isOK)
	assert.Equal(t, []api.OSProfile{{Name: "nke-a"}, {Name: "nke-b"}}, ok.OsProfiles)
}

func TestListOSProfilesNoReleasesReturnsEmptyList(t *testing.T) {
	t.Parallel()

	resp, err := NewOSProfileService(slog.Default(), releaseClient(t)).ListOSProfiles(t.Context(), api.ListOSProfilesRequestObject{})
	require.NoError(t, err)
	ok, isOK := resp.(api.ListOSProfiles200JSONResponse)
	require.True(t, isOK)
	assert.NotNil(t, ok.OsProfiles)
	assert.Empty(t, ok.OsProfiles)
}

func TestListOSProfilesFailsWithoutReaderOrOnListError(t *testing.T) {
	t.Parallel()

	_, err := NewOSProfileService(slog.Default(), nil).ListOSProfiles(t.Context(), api.ListOSProfilesRequestObject{})
	require.ErrorContains(t, err, "release reader is not configured")

	_, err = NewOSProfileService(slog.Default(), failingListClient(t, errors.New("boom"))).ListOSProfiles(t.Context(), api.ListOSProfilesRequestObject{})
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
