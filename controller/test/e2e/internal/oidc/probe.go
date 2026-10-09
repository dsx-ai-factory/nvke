// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	maxProbeBody = 1 << 20
)

// EndpointTrust is the server address and CA data from an admin kubeconfig.
// It carries no admin credential fields.
type EndpointTrust struct {
	Endpoint string
	CAData   []byte
}

// ProbeResult records the identity and the two real HTTP authorization results.
type ProbeResult struct {
	IdentityStatus int
	Username       string
	Groups         []string
	AllowedStatus  int
	DeniedStatus   int
}

// TrustFromKubeconfig extracts only the active cluster's endpoint and CA data.
func TrustFromKubeconfig(data []byte) (EndpointTrust, error) {
	config, err := clientcmd.Load(data)
	if err != nil {
		return EndpointTrust{}, errors.New("decode tenant kubeconfig: invalid kubeconfig")
	}
	contextConfig, ok := config.Contexts[config.CurrentContext]
	if !ok || contextConfig == nil {
		return EndpointTrust{}, errors.New("tenant kubeconfig has no current context")
	}
	cluster, ok := config.Clusters[contextConfig.Cluster]
	if !ok || cluster == nil {
		return EndpointTrust{}, errors.New("tenant kubeconfig current context has no cluster")
	}
	if cluster.Server == "" || len(cluster.CertificateAuthorityData) == 0 {
		return EndpointTrust{}, errors.New("tenant kubeconfig cluster needs a server and inline CA data")
	}
	return EndpointTrust{Endpoint: cluster.Server, CAData: bytes.Clone(cluster.CertificateAuthorityData)}, nil
}

// ProbeToken builds a fresh bearer-only client and makes identity, allowed, and denied requests.
func ProbeToken(ctx context.Context, endpoint string, ca, token []byte, allowedPath, deniedPath string) (ProbeResult, error) {
	if len(token) == 0 {
		return ProbeResult{}, errors.New("OIDC token is empty")
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return ProbeResult{}, errors.New("parse tenant endpoint URL: invalid URL")
	}
	if base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return ProbeResult{}, errors.New("tenant endpoint must be an absolute HTTPS URL")
	}
	if len(ca) == 0 {
		return ProbeResult{}, errors.New("tenant CA data is empty")
	}
	for _, path := range []string{allowedPath, deniedPath} {
		if !strings.HasPrefix(path, "/") {
			return ProbeResult{}, errors.New("probe paths must start with a slash")
		}
	}
	config := &rest.Config{
		Host:            endpoint,
		TLSClientConfig: rest.TLSClientConfig{CAData: bytes.Clone(ca)},
		BearerToken:     string(token),
		Proxy:           http.ProxyURL(nil),
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("build bearer-only tenant transport: %w", err)
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	result := ProbeResult{}
	identity := authenticationv1.SelfSubjectReview{}
	identity.APIVersion = "authentication.k8s.io/v1"
	identity.Kind = "SelfSubjectReview"
	payload, err := json.Marshal(identity)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("encode identity request: %w", err)
	}
	identityURL := *base
	identityURL.Path = "/apis/authentication.k8s.io/v1/selfsubjectreviews"
	response, body, err := probeRequest(ctx, client, http.MethodPost, identityURL.String(), payload)
	if err != nil {
		return ProbeResult{}, err
	}
	result.IdentityStatus = response
	if response >= 200 && response < 300 {
		var review authenticationv1.SelfSubjectReview
		if err = json.Unmarshal(body, &review); err != nil {
			return ProbeResult{}, fmt.Errorf("decode identity response: %w", err)
		}
		result.Username = review.Status.UserInfo.Username
		result.Groups = append([]string(nil), review.Status.UserInfo.Groups...)
	}

	allowedURL := *base
	allowedURL.Path = allowedPath
	result.AllowedStatus, _, err = probeRequest(ctx, client, http.MethodGet, allowedURL.String(), nil)
	if err != nil {
		return ProbeResult{}, err
	}
	deniedURL := *base
	deniedURL.Path = deniedPath
	result.DeniedStatus, _, err = probeRequest(ctx, client, http.MethodGet, deniedURL.String(), nil)
	if err != nil {
		return ProbeResult{}, err
	}
	return result, nil
}

func probeRequest(ctx context.Context, client *http.Client, method, endpoint string, body []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build tenant API request: %w", err)
	}
	if len(body) != 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("call tenant API: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxProbeBody+1))
	if err != nil {
		return 0, nil, fmt.Errorf("read tenant API response: %w", err)
	}
	if len(responseBody) > maxProbeBody {
		return 0, nil, errors.New("tenant API response exceeds read limit")
	}
	return response.StatusCode, responseBody, nil
}
