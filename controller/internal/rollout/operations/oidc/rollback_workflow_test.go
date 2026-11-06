// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	adapter "github.com/dsx-ai-factory/nvke/controller/internal/rollout/adapters/controller"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/oidc"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
)

type clusterLoader struct{ cluster rollout.Cluster }

func (l clusterLoader) LoadCluster(context.Context, uuid.UUID) (rollout.Cluster, error) {
	return l.cluster, nil
}

type recordingAgent struct {
	nvkeproto.NodeAgentClient
	outcome nvkeproto.PatchAPIServerManifestResponse_Outcome
	calls   []*nvkeproto.PatchAPIServerManifestRequest
}

func (a *recordingAgent) PatchAPIServerManifest(_ context.Context, req *nvkeproto.PatchAPIServerManifestRequest, _ ...grpc.CallOption) (*nvkeproto.PatchAPIServerManifestResponse, error) {
	a.calls = append(a.calls, req)
	return &nvkeproto.PatchAPIServerManifestResponse{Outcome: a.outcome, ErrorMessage: "injected later-node failure"}, nil
}

func TestRollout_LaterNodeFailureRestoresAuthentication(t *testing.T) {
	for _, tt := range []struct {
		name      string
		outcome   nvkeproto.PatchAPIServerManifestResponse_Outcome
		providers bool
	}{
		{name: "operator failed restored", outcome: nvkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED, providers: true},
		{name: "operator failed not restored", outcome: nvkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED, providers: true},
		{name: "anonymous failed restored", outcome: nvkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED},
		{name: "anonymous failed not restored", outcome: nvkeproto.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := kubeadmconfig.OperatorJWTPolicy{
				ManagementJWTTrustDomain: "jwt.example.com",
				ReservedSubjects:         []string{"spiffe://jwt.example.com/capi"},
			}
			provider := controllerconfig.OIDCProvider{
				Name: "workforce", IssuerURL: "https://workforce.example.com", Audiences: []string{"tenant"},
				UsernameClaim: "sub", UsernamePrefix: "workforce:", GroupsClaim: "groups", GroupsPrefix: "workforce:",
				RequiredClaims: map[string]string{"organization": "example"}, CABundle: "example public CA bundle",
			}
			row := db.Cluster{
				ID: uuid.New(), Name: "tenant", KubernetesVersion: "v1.34.2", OIDCAuthEnabled: false,
			}
			if tt.providers {
				row.OIDCProviders = db.ClusterOIDCProvidersFromConfig([]controllerconfig.OIDCProvider{provider})
			}
			nodes := []db.Node{
				{ID: uuid.New(), Name: "cp-0", ControlPlane: true},
				{ID: uuid.New(), Name: "cp-1", ControlPlane: true},
				{ID: uuid.New(), Name: "cp-2", ControlPlane: true},
			}
			cluster := adapter.NewCluster(row, nodes, nodes, "50051")
			first := &recordingAgent{outcome: nvkeproto.PatchAPIServerManifestResponse_OUTCOME_APPLIED}
			later := &recordingAgent{outcome: tt.outcome}
			untouched := &recordingAgent{outcome: nvkeproto.PatchAPIServerManifestResponse_OUTCOME_APPLIED}
			agents := map[string]*recordingAgent{"cp-0": first, "cp-1": later, "cp-2": untouched}
			op := oidc.NewEnableOIDCOperation(
				oidc.WithClient(fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
					Data:       map[string]string{nvkerbac.ConfigMapKey: "kind: ClusterRoleBinding\nsubjects:\n  - {kind: User, name: spiffe://jwt.example.com/capi}\n"},
				}).Build()),
				oidc.WithRBACConfigMap(nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName}),
				oidc.WithTrustDomain(policy.ManagementJWTTrustDomain),
				oidc.WithOIDCConfig(&config.NvkeOIDCAuth{Enabled: true, IssuerURL: "https://shared.example.com"}),
				oidc.WithMethodSupportChecker(func(context.Context, rollout.Node, string) error { return nil }),
				oidc.WithDialer(func(_ context.Context, node rollout.Node) (nvkeproto.NodeAgentClient, func(), error) {
					return agents[node.Name()], func() {}, nil
				}),
			)
			registry := rollout.NewRegistry()
			registry.RegisterOperation(rollout.Compile(op))
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			rollouttemporal.NewActivities(clusterLoader{cluster}, registry).Register(env)
			wf := func(ctx workflow.Context) (rollout.Result, error) {
				ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
				return rollouttemporal.Execute(ctx, cluster, rollout.Compile(op), rollout.RolloutSpec{
					Strategy: rollout.StrategySerial, RollbackPolicy: rollout.RollbackHaltOnAnyFailure,
				})
			}
			env.ExecuteWorkflow(wf)
			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			var result rollout.Result
			require.NoError(t, env.GetWorkflowResult(&result))
			require.Error(t, result.Err())
			require.Len(t, first.calls, 2, "first node must apply then roll back")
			require.Len(t, later.calls, 1, "the failing node is not rolled back by the cascade")
			require.Empty(t, untouched.calls, "failure must halt the rollout")

			previous := kubeadmconfig.BuildAnonymousAuthenticationConfig(row.KubernetesVersion)
			if tt.providers {
				var err error
				previous, err = kubeadmconfig.BuildCombinedAuthenticationConfig(row.Name,
					controllerconfig.ManagementOIDCAuthConfig{}, nil, []controllerconfig.OIDCProvider{provider}, policy)
				require.NoError(t, err)
			}
			file, err := kubeadmconfig.GenerateAPIServerAuthenticationConfig(previous, row.KubernetesVersion)
			require.NoError(t, err)
			restored := first.calls[1]
			require.Len(t, restored.GetFiles(), 1, "rollback must restore the bootstrap auth file")
			require.Equal(t, file.Content, restored.GetFiles()[0].GetContent())
			require.Equal(t, file.Path, restored.GetFiles()[0].GetPath())
			require.Equal(t, file.Mode, restored.GetFiles()[0].GetMode())
			require.Equal(t, file.Owner, restored.GetFiles()[0].GetOwner())
			require.Empty(t, restored.GetRemoveFiles())
			require.Len(t, restored.GetFlagOps(), 1)
			require.Equal(t, file.Path, restored.GetFlagOps()[0].GetSet().GetValue())
			require.Equal(t, first.calls[0].GetStrategicMergePatches(), restored.GetStrategicMergePatches(),
				"rollback must keep the authentication mount and volume")

			var enabled kubeadmconfig.OIDCAuthenticationConfig
			require.NoError(t, yaml.Unmarshal(first.calls[0].GetFiles()[0].GetContent(), &enabled))
			require.Len(t, enabled.JWT, len(row.OIDCProviders)+1)
			require.Equal(t, "https://shared.example.com", enabled.JWT[0].Issuer.URL)
			if tt.providers {
				require.Equal(t, previous.JWT[0], enabled.JWT[1])
			}
		})
	}
}
