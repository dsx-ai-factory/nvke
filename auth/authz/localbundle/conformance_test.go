// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz/authztest"
	"github.com/dsx-ai-factory/nvke/auth/authz/localbundle"
)

const operatorTenant = "nca-0000000000000000"

// conformanceBundle allows getCluster and denies deleteCluster to every
// caller. Keycloak callers get their tenant from a claim. Entra callers in the
// operator tenant are operators, and so is every cluster-sa caller.
// readers-may-get is there because the load refuses a role no policy names.
func conformanceBundle() files {
	return files{
		"policies.cedar": `
@id("anyone-may-get")
permit(principal, action == NVKE::Action::"getCluster", resource);

@id("readers-may-get")
permit(principal in NVKE::Role::"reader", action == NVKE::Action::"getCluster", resource);
`,
		"roles.json": `[` + reader + `]`,
		"authorization.yaml": configHeader + `operatorTenant: ` + operatorTenant + `
issuers:
  keycloak:
    tenant: {claim: organization}
  entra:
    tenant: {claim: tid}
    operators: true
  cluster-sa:
    tenant: {fixed: ` + operatorTenant + `}
    operators: true
`,
	}
}

func newConformanceBundle(dir string) (*localbundle.LocalBundle, error) {
	return localbundle.New(localbundle.Options{
		Dir:           dir,
		Issuers:       []string{"keycloak", "entra", "cluster-sa", "github"},
		Actions:       []string{"getCluster", "deleteCluster"},
		ResourceTypes: []string{"Cluster", "NodePool", "Collection"},
	})
}

// keycloakIn is a Keycloak caller with organization set to tenant, or with no
// organization claim when tenant is empty.
func keycloakIn(subject, tenant string) authn.Principal {
	claims := map[string]any{}
	if tenant != "" {
		claims["organization"] = tenant
	}
	return authn.Principal{IssuerName: "keycloak", Subject: subject, Claims: claims}
}

func TestConformance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundle(t, dir, conformanceBundle())
	lb, err := newConformanceBundle(dir)
	require.NoError(t, err)

	impostor := keycloakIn("mallory", operatorTenant)
	operatorNoTenant := authn.Principal{IssuerName: "entra", Subject: "ops-bot", Claims: map[string]any{}}
	authztest.Run(t, authztest.Fixture{
		Authorizer:       lb,
		AllowedAction:    "getCluster",
		DeniedAction:     "deleteCluster",
		TenantA:          "tenant-a",
		TenantB:          "tenant-b",
		InTenantA:        keycloakIn("alice", "tenant-a"),
		InTenantB:        keycloakIn("bob", "tenant-b"),
		SameSubjectInB:   keycloakIn("alice", "tenant-b"),
		Operator:         authn.Principal{IssuerName: "cluster-sa", Subject: "system:serviceaccount:nvke-ops:fleet"},
		NoTenant:         keycloakIn("carol", ""),
		Impostor:         &impostor,
		OperatorNoTenant: &operatorNoTenant,
	})
}
