// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPOutcome(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusOK, "succeeded"},
		{http.StatusCreated, "succeeded"},
		{http.StatusNotModified, "succeeded"},
		{http.StatusBadRequest, "rejected"},
		{http.StatusNotFound, "rejected"},
		{http.StatusConflict, "rejected"},
		{http.StatusInternalServerError, "failed"},
		{http.StatusBadGateway, "failed"},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			require.Equal(t, tt.want, httpOutcome(tt.status))
		})
	}
}

func TestHTTPRejectionReason(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusOK, ""},
		{http.StatusBadRequest, "bad_request"},
		{http.StatusUnauthorized, "unauthorized"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusNotFound, "not_found"},
		{http.StatusConflict, "conflict"},
		{http.StatusUnprocessableEntity, "unprocessable_entity"},
		{http.StatusTooManyRequests, "too_many_requests"},
		{http.StatusTeapot, "client_error"},
		{http.StatusInternalServerError, ""},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			require.Equal(t, tt.want, httpRejectionReason(tt.status))
		})
	}
}

func TestResponseWriterRetainsFirstFinalStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := &requestLogState{}
	writer := responseWriter{ResponseWriter: recorder, status: http.StatusOK, requestLogState: state}

	writer.WriteHeader(http.StatusConflict)
	writer.WriteHeader(http.StatusEarlyHints)
	writer.WriteHeader(http.StatusInternalServerError)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Equal(t, http.StatusConflict, writer.status)
	require.Equal(t, "conflict", state.rejectionReason)
}

func TestResponseWriterTreatsSwitchingProtocolsAsFinal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := responseWriter{ResponseWriter: recorder, status: http.StatusOK, requestLogState: &requestLogState{}}

	writer.WriteHeader(http.StatusSwitchingProtocols)
	writer.WriteHeader(http.StatusOK)

	require.Equal(t, http.StatusSwitchingProtocols, recorder.Code)
	require.Equal(t, http.StatusSwitchingProtocols, writer.status)
}

func TestResponseWriterRetainsImplicitSuccessStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := &requestLogState{}
	writer := responseWriter{ResponseWriter: recorder, status: http.StatusOK, requestLogState: state}

	_, err := writer.Write([]byte("ok"))
	require.NoError(t, err)
	writer.WriteHeader(http.StatusConflict)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, http.StatusOK, writer.status)
	require.Empty(t, state.rejectionReason)
}

func TestResponseWriterRetainsSpecificRejectionReason(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := &requestLogState{}
	writer := responseWriter{ResponseWriter: recorder, status: http.StatusOK, requestLogState: state}

	writer.recordRejectionReason(rejectionReasonInvalidClusterNetwork)
	writer.WriteHeader(http.StatusBadRequest)

	require.Equal(t, rejectionReasonInvalidClusterNetwork, state.rejectionReason)
}
