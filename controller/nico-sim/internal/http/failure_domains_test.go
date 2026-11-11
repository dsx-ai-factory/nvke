// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"encoding/json"
	nethttp "net/http"
	"testing"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testFailureDomainLabelKey = "failure_domain"

var testFailureDomains = FailureDomainConfig{
	LabelKey:    testFailureDomainLabelKey,
	Domains:     []string{"fd-a", "fd-b", "fd-c"},
	Unavailable: []string{"fd-a"},
}

func configureFailureDomains(t *testing.T, handler nethttp.Handler, config FailureDomainConfig) {
	t.Helper()
	response := performJSONRequest(t, handler, nethttp.MethodPut, "/sim/failure-domains", config)
	require.Equal(t, nethttp.StatusOK, response.Code, response.Body.String())
}

func TestUnit_PutFailureDomains(t *testing.T) {
	tests := []struct {
		name       string
		config     FailureDomainConfig
		wantStatus int
	}{
		{name: "accepts domains with one unavailable", config: testFailureDomains, wantStatus: nethttp.StatusOK},
		{name: "accepts clearing the layout", config: FailureDomainConfig{}, wantStatus: nethttp.StatusOK},
		{name: "rejects domains without a label key", config: FailureDomainConfig{Domains: []string{"fd-a"}}, wantStatus: nethttp.StatusBadRequest},
		{name: "rejects duplicate domains", config: FailureDomainConfig{LabelKey: "k", Domains: []string{"fd-a", "fd-a"}}, wantStatus: nethttp.StatusBadRequest},
		{name: "rejects an unknown unavailable domain", config: FailureDomainConfig{LabelKey: "k", Domains: []string{"fd-a"}, Unavailable: []string{"fd-z"}}, wantStatus: nethttp.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newDispatchHandler(t, newUnifiedTestStore(t), &fakeContainerService{instances: map[string]nicosdk.Instance{}})
			response := performJSONRequest(t, handler, nethttp.MethodPut, "/sim/failure-domains", tt.config)
			require.Equal(t, tt.wantStatus, response.Code, response.Body.String())
		})
	}
}

func TestUnit_ListMachinesPublishesFailureDomains(t *testing.T) {
	handler := newDispatchHandler(t, newUnifiedTestStore(t), &fakeContainerService{instances: map[string]nicosdk.Instance{}})
	configureFailureDomains(t, handler, testFailureDomains)

	tests := []struct {
		name string
		page string
		want []string
	}{
		{name: "first page lists every domain, unavailable included", page: "1", want: []string{"fd-a", "fd-b", "fd-c"}},
		{name: "later pages are empty", page: "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/v2/org/" + dispatchTestOrgID + "/carbide/machine?siteId=site-1&hasInstanceType=true&pageSize=100&pageNumber=" + tt.page
			response := performJSONRequest(t, handler, nethttp.MethodGet, path, nil)
			require.Equal(t, nethttp.StatusOK, response.Code, response.Body.String())
			var machines []nicosdk.Machine
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &machines))
			var got []string
			for _, machine := range machines {
				assert.Equal(t, "site-1", machine.GetSiteId())
				got = append(got, machine.GetLabels()[testFailureDomainLabelKey])
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUnit_CreateInstanceWithMachineLabelSelector(t *testing.T) {
	tests := []struct {
		name       string
		config     FailureDomainConfig
		selector   map[string]string
		wantStatus int
		wantLabels map[string]string
	}{
		{
			name: "places the instance in an available domain", config: testFailureDomains,
			selector:   map[string]string{testFailureDomainLabelKey: "fd-b"},
			wantStatus: nethttp.StatusOK, wantLabels: map[string]string{testFailureDomainLabelKey: "fd-b"},
		},
		{
			name: "rejects an unavailable domain", config: testFailureDomains,
			selector: map[string]string{testFailureDomainLabelKey: "fd-a"}, wantStatus: nethttp.StatusBadRequest,
		},
		{
			name: "rejects an unknown domain", config: testFailureDomains,
			selector: map[string]string{testFailureDomainLabelKey: "fd-z"}, wantStatus: nethttp.StatusBadRequest,
		},
		{
			name: "rejects a selector on another label", config: testFailureDomains,
			selector: map[string]string{"rack": "fd-b"}, wantStatus: nethttp.StatusBadRequest,
		},
		{
			name: "ignores the selector when no domains are configured", config: FailureDomainConfig{},
			selector: map[string]string{testFailureDomainLabelKey: "fd-a"}, wantStatus: nethttp.StatusOK,
		},
		{
			name: "creates unpinned instances with domains configured", config: testFailureDomains,
			wantStatus: nethttp.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers := &fakeContainerService{instances: map[string]nicosdk.Instance{}}
			handler := newDispatchHandler(t, newUnifiedTestStore(t), containers)
			configureFailureDomains(t, handler, tt.config)

			request := containerCreateRequest(testContainerInstanceType)
			if tt.selector != nil {
				request.SetMachineLabelSelector(tt.selector)
			}
			response := performJSONRequest(t, handler, nethttp.MethodPost, "/v2/org/"+dispatchTestOrgID+"/forge/instance", request)
			require.Equal(t, tt.wantStatus, response.Code, response.Body.String())
			if tt.wantStatus != nethttp.StatusOK {
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				assert.Equal(t, noMachinesAvailable, body["message"])
				assert.Empty(t, containers.instances, "a rejected create must not provision")
				return
			}

			machineID := "machine-" + containers.lastID[:8]
			response = performJSONRequest(t, handler, nethttp.MethodGet, "/v2/org/"+dispatchTestOrgID+"/carbide/machine/"+machineID, nil)
			require.Equal(t, nethttp.StatusOK, response.Code, response.Body.String())
			var machine nicosdk.Machine
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &machine))
			assert.Equal(t, tt.wantLabels, machine.Labels)
		})
	}
}
