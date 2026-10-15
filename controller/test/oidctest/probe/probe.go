// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package probe makes token-only requests to a Kubernetes API server.
package probe

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ResourceRequest identifies a namespaced list request used for RBAC evidence.
type ResourceRequest struct {
	Resource  string
	Namespace string
}

// Request contains the complete, token-only connection state for one probe.
type Request struct {
	Endpoint string
	CAPEM    []byte
	Token    string
	Allowed  ResourceRequest
	Denied   ResourceRequest
}

// Outcome records one Kubernetes resource request.
type Outcome struct {
	HTTPStatus int
	Allowed    bool
}

// Result separates an authentication rejection from probe setup failures.
type Result struct {
	HTTPStatus int
	Username   string
	Groups     []string
	Allowed    *Outcome
	Denied     *Outcome
}

// Run authenticates the token, reads the resulting identity, and exercises RBAC.
func Run(ctx context.Context, request Request) (Result, error) {
	if err := validateResource(request.Allowed); err != nil {
		return Result{}, fmt.Errorf("allowed request: %w", err)
	}
	if err := validateResource(request.Denied); err != nil {
		return Result{}, fmt.Errorf("denied request: %w", err)
	}
	config, err := NewTokenOnlyConfig(request)
	if err != nil {
		return Result{}, err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return Result{}, fmt.Errorf("create Kubernetes client: %w", err)
	}
	review, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		var status apierrors.APIStatus
		if errors.As(err, &status) {
			return Result{HTTPStatus: int(status.Status().Code)}, nil
		}
		return Result{}, fmt.Errorf("read authenticated identity: %w", err)
	}
	result := Result{HTTPStatus: http.StatusOK, Username: review.Status.UserInfo.Username, Groups: review.Status.UserInfo.Groups}
	allowed, err := readResource(ctx, client, request.Allowed)
	if err != nil {
		return Result{}, err
	}
	result.Allowed = &allowed
	denied, err := readResource(ctx, client, request.Denied)
	if err != nil {
		return Result{}, err
	}
	result.Denied = &denied
	return result, nil
}

// NewTokenOnlyConfig creates a rest.Config without inherited credentials,
// client certificates, exec plugins, or impersonation.
func NewTokenOnlyConfig(request Request) (*rest.Config, error) {
	endpoint, err := url.ParseRequestURI(request.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("endpoint must be an absolute HTTPS URL")
	}
	if request.Token == "" {
		return nil, errors.New("bearer token is required")
	}
	roots := x509.NewCertPool()
	if len(request.CAPEM) == 0 || !roots.AppendCertsFromPEM(request.CAPEM) {
		return nil, errors.New("CA PEM must contain a certificate")
	}
	return &rest.Config{
		Host: request.Endpoint, BearerToken: request.Token, Proxy: http.ProxyURL(nil),
		TLSClientConfig: rest.TLSClientConfig{CAData: append([]byte(nil), request.CAPEM...)},
	}, nil
}

func readResource(ctx context.Context, client kubernetes.Interface, request ResourceRequest) (Outcome, error) {
	response := client.CoreV1().RESTClient().Get().Namespace(request.Namespace).Resource(request.Resource).Do(ctx)
	statusCode := 0
	response.StatusCode(&statusCode)
	if err := response.Error(); err != nil {
		var status apierrors.APIStatus
		if errors.As(err, &status) {
			return Outcome{HTTPStatus: int(status.Status().Code)}, nil
		}
		return Outcome{}, fmt.Errorf("list %s: %w", request.Resource, err)
	}
	return Outcome{HTTPStatus: statusCode, Allowed: true}, nil
}

func validateResource(request ResourceRequest) error {
	if request.Namespace == "" || request.Resource == "" {
		return errors.New("namespace and resource are required")
	}
	return nil
}
