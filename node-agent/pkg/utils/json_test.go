package utils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFetchJSON(t *testing.T) {
	type TestData struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"test-bundle","version":"1.0.0"}`)) //nolint:errcheck
	}))
	defer server.Close()

	var result TestData
	err := FetchJSON(context.Background(), server.URL, &result)

	assert.NoError(t, err)
	assert.Equal(t, "test-bundle", result.Name)
	assert.Equal(t, "1.0.0", result.Version)
}
