// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
)

// SupportsAuthenticationConfiguration reports whether kube-apiserver at
// kubernetesVersion can read the v1 AuthenticationConfiguration this package
// renders, which needs 1.34 or later.
func SupportsAuthenticationConfiguration(kubernetesVersion string) bool {
	return supportsAnonymousAuthConfig(kubernetesVersion)
}

// ClusterAuthentication is what a cluster's AuthenticationConfiguration is built from.
type ClusterAuthentication struct {
	ClusterName   string
	Shared        config.NvkeOIDCAuth
	SharedEnabled bool
	Providers     []controllerconfig.OIDCProvider
	TrustDomain   string
	// RBACConfigMap is the operator-managed RBAC ConfigMap whose User subjects
	// are reserved (config.Config.RBACConfigMapSource).
	RBACConfigMap nvkerbac.Source
}

// Needed reports whether the cluster has any OIDC provider.
func (c ClusterAuthentication) Needed() bool {
	return c.SharedEnabled || len(c.Providers) > 0
}

// BuildClusterAuthenticationConfig builds a cluster's AuthenticationConfiguration.
// A cluster with only the shared provider gets exactly what
// BuildOIDCAuthenticationConfig builds.
func BuildClusterAuthenticationConfig(ctx context.Context, c client.Client, in ClusterAuthentication) (*OIDCAuthenticationConfig, error) {
	if !in.Needed() {
		return nil, errors.New("the cluster uses neither the shared OIDC provider nor fleet-wide providers")
	}

	var caBundle []byte
	if in.SharedEnabled {
		if ref := in.Shared.CABundleSecret; !ref.Empty() {
			var err error
			caBundle, err = ReadOIDCCABundle(ctx, c, ref)
			if err != nil {
				return nil, err
			}
		}
		if len(in.Providers) == 0 {
			return BuildOIDCAuthenticationConfig(in.ClusterName, in.Shared, caBundle)
		}
	}

	policy, err := LoadOperatorJWTPolicy(ctx, c, in.TrustDomain, in.RBACConfigMap)
	if err != nil {
		return nil, err
	}
	return BuildCombinedAuthenticationConfig(in.ClusterName, controllerconfig.ManagementOIDCAuthConfig{
		Enabled:      in.SharedEnabled,
		IssuerURL:    in.Shared.IssuerURL,
		DiscoveryURL: in.Shared.DiscoveryURL,
	}, caBundle, in.Providers, policy)
}

// LoadOperatorJWTPolicy reserves the management SPIFFE trust domain and the User
// subjects the RBAC manifest binds. With the manifest present, the trust domain
// is required; with the trust domain set, the manifest is required, since the
// chart renders it and a missing one would otherwise silently reserve nothing.
func LoadOperatorJWTPolicy(ctx context.Context, reader client.Reader, trustDomain string, src nvkerbac.Source) (OperatorJWTPolicy, error) {
	if reader == nil {
		return OperatorJWTPolicy{}, errors.New("reading the RBAC manifest needs a management-cluster client")
	}
	policy := OperatorJWTPolicy{ManagementJWTTrustDomain: trustDomain}

	manifest, err := nvkerbac.ReadManifest(ctx, reader, src)
	if apierrors.IsNotFound(err) {
		if trustDomain != "" {
			return OperatorJWTPolicy{}, fmt.Errorf("the identities fleet-wide OIDC providers must not claim are unknown: %w", err)
		}
		return policy, nil
	}
	if err != nil {
		return OperatorJWTPolicy{}, fmt.Errorf("reading the identities fleet-wide OIDC providers must not claim: %w", err)
	}
	if trustDomain == "" {
		return OperatorJWTPolicy{}, fmt.Errorf("ConfigMap %s/%s binds NVKE identities, but the management SPIFFE trust domain is unknown", src.Namespace, src.Name)
	}

	policy.ReservedSubjects, err = nvkerbac.UserSubjects(manifest)
	if err != nil {
		return OperatorJWTPolicy{}, err
	}
	return policy, nil
}
