// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderemediation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func nodeWith(labels, annotations map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: labels, Annotations: annotations}}
}

// SREs and tooling such as NVSentinel raise remediation requests with the
// label domain they know. A request under the legacy domain must still be seen
// when a different domain is configured, and a request under an unrelated
// domain must not be.
func TestNodeHasRemediationRequestReadsBothDomains(t *testing.T) {
	t.Parallel()

	custom := keys.New("nvke.example.com", "")

	assert.True(t, nodeHasRemediationRequest(custom, nodeWith(map[string]string{"nke.nvidia.com/reboot": "gpu-xid"}, nil)), "legacy reboot")
	assert.True(t, nodeHasRemediationRequest(custom, nodeWith(map[string]string{"nvke.example.com/reboot": "gpu-xid"}, nil)), "configured reboot")
	assert.True(t, nodeHasRemediationRequest(custom, nodeWith(map[string]string{"nke.nvidia.com/terminate": "bad"}, nil)), "legacy terminate")
	assert.True(t, nodeHasRemediationRequest(custom, nodeWith(nil, map[string]string{"nke.nvidia.com/machine-health-issue": "{}"})), "legacy health issue")
	assert.False(t, nodeHasRemediationRequest(custom, nodeWith(map[string]string{"other.example.com/reboot": "x"}, nil)), "unrelated domain")

	// The default domain has no second form, so only its own key counts.
	assert.True(t, nodeHasRemediationRequest(keys.New("", ""), nodeWith(map[string]string{"nke.nvidia.com/reboot": "x"}, nil)))
	assert.False(t, nodeHasRemediationRequest(keys.New("", ""), nodeWith(map[string]string{"nvke.example.com/reboot": "x"}, nil)))
}

func TestNodeRemediationRequestChangedSeesLegacyToConfigured(t *testing.T) {
	t.Parallel()

	custom := keys.New("nvke.example.com", "")

	legacy := nodeWith(map[string]string{"nke.nvidia.com/reboot": "gpu-xid"}, nil)
	sameValueConfigured := nodeWith(map[string]string{"nvke.example.com/reboot": "gpu-xid"}, nil)
	changedValue := nodeWith(map[string]string{"nke.nvidia.com/reboot": "other"}, nil)

	// Moving a request between forms without changing its value is not a new request.
	assert.False(t, nodeRemediationRequestChanged(custom, legacy, sameValueConfigured))
	assert.True(t, nodeRemediationRequestChanged(custom, legacy, changedValue))
}
