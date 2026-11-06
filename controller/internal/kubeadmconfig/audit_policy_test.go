// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit/policy"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
)

var (
	auditAdmin = &user.DefaultInfo{Name: "alice@example.com", Groups: []string{user.AllAuthenticated}}
	auditNode  = &user.DefaultInfo{Name: "system:node:cp-1", Groups: []string{user.NodesGroup, user.AllAuthenticated}}
	auditProbe = &user.DefaultInfo{Name: user.Anonymous, Groups: []string{user.AllUnauthenticated}}
)

func auditServiceAccount(namespace, name string) user.Info {
	return &user.DefaultInfo{
		Name:   "system:serviceaccount:" + namespace + ":" + name,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, user.AllAuthenticated},
	}
}

func auditResourceRequest(u user.Info, verb, group, resource, subresource string) authorizer.AttributesRecord {
	return authorizer.AttributesRecord{
		User:            u,
		Verb:            verb,
		APIGroup:        group,
		Resource:        resource,
		Subresource:     subresource,
		Namespace:       "default",
		ResourceRequest: true,
	}
}

// TestAPIServerAuditPolicyLevels evaluates the shipped audit policy with the
// API server's own rule evaluator, so each case asserts the level a real
// request would be logged at rather than the shape of the YAML.
func TestAPIServerAuditPolicyLevels(t *testing.T) {
	loaded, err := policy.LoadPolicyFromBytes([]byte(apiServerAuditPolicyContent))
	require.NoError(t, err)
	evaluator := policy.NewPolicyRuleEvaluator(loaded)

	kubeSystemController := auditServiceAccount("kube-system", "replicaset-controller")
	// kubeadm runs the controller manager with per-controller credentials, so
	// the controller that approves and signs CSRs is this service account.
	certificateController := auditServiceAccount("kube-system", "certificate-controller")

	tests := []struct {
		name  string
		attrs authorizer.AttributesRecord
		want  audit.Level
	}{
		{
			name:  "a kube-system service account creating a cluster role binding is logged with its request",
			attrs: auditResourceRequest(kubeSystemController, "create", "rbac.authorization.k8s.io", "clusterrolebindings", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user creating a cluster role binding is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "create", "rbac.authorization.k8s.io", "clusterrolebindings", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user patching a role is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "patch", "rbac.authorization.k8s.io", "roles", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user creating a mutating webhook is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "create", "admissionregistration.k8s.io", "mutatingwebhookconfigurations", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a kube-system service account updating an admission policy binding is logged with its request",
			attrs: auditResourceRequest(kubeSystemController, "update", "admissionregistration.k8s.io", "validatingadmissionpolicybindings", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "an operator service account creating a cluster role binding is logged with its request",
			attrs: auditResourceRequest(auditServiceAccount("kubevirt", "kubevirt-operator"), "create", "rbac.authorization.k8s.io", "clusterrolebindings", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a kube-system service account creating a secret is logged",
			attrs: auditResourceRequest(kubeSystemController, "create", "", "secrets", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a monitoring service account patching a secret is logged",
			attrs: auditResourceRequest(auditServiceAccount("monitoring", "prometheus-operator"), "patch", "", "secrets", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a kube-system service account exec into a pod is logged",
			attrs: auditResourceRequest(kubeSystemController, "create", "", "pods", "exec"),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a request proxied to a kubelet through the API server is logged",
			attrs: auditResourceRequest(auditServiceAccount("monitoring", "prometheus"), "get", "", "nodes", "proxy"),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user adding an ephemeral container is logged",
			attrs: auditResourceRequest(auditAdmin, "patch", "", "pods", "ephemeralcontainers"),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user approving a certificate signing request is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "update", "certificates.k8s.io", "certificatesigningrequests", "approval"),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user creating a network policy is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "create", "networking.k8s.io", "networkpolicies", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user writing a Calico global network policy through its CRD is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "update", "crd.projectcalico.org", "globalnetworkpolicies", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user creating a Calico tier through its CRD is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "create", "crd.projectcalico.org", "tiers", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user patching a Calico global network set through the Calico API server is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "patch", "projectcalico.org", "globalnetworksets", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user creating a Calico tier through the Calico API server is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "create", "projectcalico.org", "tiers", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user patching a Calico network policy through the Calico API server is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "patch", "projectcalico.org", "networkpolicies", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user creating a daemon set is logged with its request",
			attrs: auditResourceRequest(auditAdmin, "create", "apps", "daemonsets", ""),
			want:  audit.LevelRequest,
		},
		{
			name:  "a user creating a token review is logged without its body, which carries the token",
			attrs: auditResourceRequest(auditAdmin, "create", "authentication.k8s.io", "tokenreviews", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user requesting a service account token is logged without its body",
			attrs: auditResourceRequest(auditAdmin, "create", "", "serviceaccounts", "token"),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user reading a secret is logged",
			attrs: auditResourceRequest(auditAdmin, "get", "", "secrets", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user creating a config map is logged",
			attrs: auditResourceRequest(auditAdmin, "create", "", "configmaps", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user creating a deployment falls through to the catch-all",
			attrs: auditResourceRequest(auditAdmin, "create", "apps", "deployments", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a node updating its own status is not logged",
			attrs: auditResourceRequest(auditNode, "patch", "", "nodes", "status"),
			want:  audit.LevelNone,
		},
		{
			name:  "a node creating a certificate signing request is not logged",
			attrs: auditResourceRequest(auditNode, "create", "certificates.k8s.io", "certificatesigningrequests", ""),
			want:  audit.LevelNone,
		},
		{
			name:  "a node requesting a service account token for a pod is not logged",
			attrs: auditResourceRequest(auditNode, "create", "", "serviceaccounts", "token"),
			want:  audit.LevelNone,
		},
		{
			name:  "a kube-system controller creating a pod is not logged",
			attrs: auditResourceRequest(kubeSystemController, "create", "", "pods", ""),
			want:  audit.LevelNone,
		},
		{
			name:  "a lease renewal is not logged",
			attrs: auditResourceRequest(auditAdmin, "update", "coordination.k8s.io", "leases", ""),
			want:  audit.LevelNone,
		},
		{
			name:  "a user listing webhooks is not logged",
			attrs: auditResourceRequest(auditAdmin, "list", "admissionregistration.k8s.io", "mutatingwebhookconfigurations", ""),
			want:  audit.LevelNone,
		},
		{
			name:  "a user listing config maps is not logged",
			attrs: auditResourceRequest(auditAdmin, "list", "", "configmaps", ""),
			want:  audit.LevelNone,
		},
		{
			name:  "a user listing cluster roles is logged, as on main",
			attrs: auditResourceRequest(auditAdmin, "list", "rbac.authorization.k8s.io", "clusterroles", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user reading a role binding is logged, as on main",
			attrs: auditResourceRequest(auditAdmin, "get", "rbac.authorization.k8s.io", "rolebindings", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user listing daemon sets is logged, as on main",
			attrs: auditResourceRequest(auditAdmin, "list", "apps", "daemonsets", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user reading a certificate signing request is logged, as on main",
			attrs: auditResourceRequest(auditAdmin, "get", "certificates.k8s.io", "certificatesigningrequests", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user listing network policies is logged, as on main",
			attrs: auditResourceRequest(auditAdmin, "list", "networking.k8s.io", "networkpolicies", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a node reading a secret is logged",
			attrs: auditResourceRequest(auditNode, "get", "", "secrets", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a kube-system service account listing secrets is logged",
			attrs: auditResourceRequest(kubeSystemController, "list", "", "secrets", ""),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a user proxying to a pod through the API server is logged",
			attrs: auditResourceRequest(auditAdmin, "get", "", "pods", "proxy"),
			want:  audit.LevelMetadata,
		},
		{
			name:  "a kube-system service account proxying to a service through the API server is logged",
			attrs: auditResourceRequest(kubeSystemController, "get", "", "services", "proxy"),
			want:  audit.LevelMetadata,
		},
		{
			name:  "the certificate controller approving a certificate signing request is logged with its request",
			attrs: auditResourceRequest(certificateController, "update", "certificates.k8s.io", "certificatesigningrequests", "approval"),
			want:  audit.LevelRequest,
		},
		{
			name:  "the certificate controller writing a signed certificate to a request's status is logged with its request",
			attrs: auditResourceRequest(certificateController, "update", "certificates.k8s.io", "certificatesigningrequests", "status"),
			want:  audit.LevelRequest,
		},
		{
			name: "a health check is not logged",
			attrs: authorizer.AttributesRecord{
				User: auditAdmin,
				Verb: "get",
				Path: "/healthz",
			},
			want: audit.LevelNone,
		},
		{
			name:  "a kubelet readiness probe of the API server is not logged",
			attrs: authorizer.AttributesRecord{User: auditProbe, Verb: "get", Path: "/readyz"},
			want:  audit.LevelNone,
		},
		{
			name:  "a single readiness check is not logged",
			attrs: authorizer.AttributesRecord{User: auditProbe, Verb: "get", Path: "/readyz/etcd"},
			want:  audit.LevelNone,
		},
		{
			name:  "a kubelet liveness probe of the API server is not logged",
			attrs: authorizer.AttributesRecord{User: auditProbe, Verb: "get", Path: "/livez"},
			want:  audit.LevelNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, evaluator.EvaluatePolicyRule(tt.attrs).Level)
		})
	}
}
