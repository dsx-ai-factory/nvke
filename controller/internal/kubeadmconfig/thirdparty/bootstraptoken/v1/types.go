/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1 contains local copies of upstream kubeadm bootstrap token types.
//
// Source (Kubernetes v1.34.0, commit f28b4c9efbca5c5c0af716d9f2d5702667ee8a45):
//
//	https://github.com/kubernetes/kubernetes/blob/v1.34.0/cmd/kubeadm/app/apis/bootstraptoken/v1/types.go
//	https://github.com/kubernetes/kubernetes/blob/v1.34.0/cmd/kubeadm/app/apis/bootstraptoken/v1/utils.go
//
// The revision is inferred; see THIRD_PARTY_NOTICES.md.
//
// Local changes:
//   - BootstrapTokenString methods are reimplemented from utils.go without
//     k8s.io/kubernetes
package v1

import (
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var bootstrapTokenRegexp = regexp.MustCompile(`^([a-z0-9]{6})\.([a-z0-9]{16})$`)

// BootstrapToken describes one bootstrap token, stored as a Secret in the cluster.
type BootstrapToken struct {
	Token *BootstrapTokenString `json:"token" datapolicy:"token"`
	// +optional
	Description string `json:"description,omitempty"`
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
	// +optional
	Expires *metav1.Time `json:"expires,omitempty"`
	// +optional
	Usages []string `json:"usages,omitempty"`
	// +optional
	Groups []string `json:"groups,omitempty"`
}

// BootstrapTokenString is a token of the format abcdef.abcdef0123456789.
type BootstrapTokenString struct {
	ID     string `json:"-"`
	Secret string `json:"-" datapolicy:"token"`
}

// MarshalJSON implements the json.Marshaler interface.
func (bts BootstrapTokenString) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", bts.String())), nil
}

// UnmarshalJSON implements the json.Unmarshaller interface.
func (bts *BootstrapTokenString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}

	token := strings.ReplaceAll(string(b), `"`, "")
	newBTS, err := NewBootstrapTokenString(token)
	if err != nil {
		return err
	}
	bts.ID = newBTS.ID
	bts.Secret = newBTS.Secret
	return nil
}

// String returns the string representation of the BootstrapTokenString.
func (bts BootstrapTokenString) String() string {
	if bts.ID != "" && bts.Secret != "" {
		return bts.ID + "." + bts.Secret
	}
	return ""
}

// NewBootstrapTokenString converts a bootstrap token string into the serialized form.
func NewBootstrapTokenString(token string) (*BootstrapTokenString, error) {
	submatches := bootstrapTokenRegexp.FindStringSubmatch(token)
	if len(submatches) != 3 {
		return nil, fmt.Errorf("the bootstrap token %q was not of the expected form", token)
	}

	return &BootstrapTokenString{
		ID:     submatches[1],
		Secret: submatches[2],
	}, nil
}
