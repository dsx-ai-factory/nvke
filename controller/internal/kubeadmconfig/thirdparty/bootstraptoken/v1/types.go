// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package v1 contains local copies of upstream kubeadm bootstrap token types.
//
// Source:
//
//	https://github.com/kubernetes/kubernetes/blob/master/cmd/kubeadm/app/apis/bootstraptoken/v1/types.go
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
