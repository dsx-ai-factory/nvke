// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// certKeyRotatingAgent models kubeadm's upload-certs: every refresh replaces
// the one certificate key of the serving node's cluster, so only the key from
// the latest refresh of that cluster still decrypts its kubeadm-certs Secret.
type certKeyRotatingAgent struct {
	*agenttest.FakeClient
	mu        sync.Mutex
	refreshes map[uuid.UUID]int
	failNext  error
	emptyNext bool
}

func newCertKeyRotatingAgent() *certKeyRotatingAgent {
	return &certKeyRotatingAgent{FakeClient: &agenttest.FakeClient{}, refreshes: map[uuid.UUID]int{}}
}

func (f *certKeyRotatingAgent) UpdateAndFetchKubeAdmJoinStrings(
	_ context.Context,
	node db.Node,
	_ *nvkeproto.KubeadmJoinStringRequest,
) (*nvkeproto.KubeadmJoinStringResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	if f.emptyNext {
		f.emptyNext = false
		return &nvkeproto.KubeadmJoinStringResponse{}, nil
	}
	f.refreshes[node.ClusterID]++
	key := certKeyFor(node.ClusterID, f.refreshes[node.ClusterID])
	worker := "kubeadm join 192.0.2.100:6443 --token example-token.REDACTED --discovery-token-ca-cert-hash sha256:REDACTED"
	return &nvkeproto.KubeadmJoinStringResponse{
		ControlplaneJoinString: worker + " --control-plane --certificate-key " + key,
		WorkerJoinString:       worker,
	}, nil
}

// currentKey is the key that last encrypted the cluster's kubeadm-certs Secret.
func (f *certKeyRotatingAgent) currentKey(clusterID uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return certKeyFor(clusterID, f.refreshes[clusterID])
}

func (f *certKeyRotatingAgent) totalRefreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.refreshes {
		total += n
	}
	return total
}

func certKeyFor(clusterID uuid.UUID, refresh int) string {
	return fmt.Sprintf("key-%s-%d", clusterID.String()[:8], refresh)
}

func certKeyFromJoinString(t *testing.T, joinString string) string {
	t.Helper()
	fields := strings.Fields(joinString)
	for i, field := range fields {
		if field == "--certificate-key" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	require.Failf(t, "no certificate key", "join string: %q", joinString)
	return ""
}

// resetJoinStringCache empties the process-wide cache before and after a test.
func resetJoinStringCache(t *testing.T) {
	t.Helper()
	emptyCache := func() {
		joinStringCache.Range(func(key, _ any) bool {
			joinStringCache.Delete(key)
			return true
		})
	}
	emptyCache()
	t.Cleanup(emptyCache)
}

// expireJoinStringCache ages every cached entry past joinStringCacheTTL.
func expireJoinStringCache() {
	joinStringCache.Range(func(_, value any) bool {
		if cached, ok := value.(*cachedJoinStrings); ok {
			cached.mu.Lock()
			cached.fetchedAt = time.Now().Add(-2 * joinStringCacheTTL)
			cached.mu.Unlock()
		}
		return true
	})
}

func joinCacheActivities(agent *certKeyRotatingAgent) *Activities {
	return &Activities{nodeAgentClient: agent}
}

func cacheServingNode(clusterID uuid.UUID, name, ip string) db.Node {
	return db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         name,
		IP:           ip,
		Conditions:   db.NodeConditions{Joined: db.Condition{Status: db.ConditionStatusTrue}},
	}
}

func TestGetKubeadmJoinStringsReturnsTheClustersCurrentCertificateKey(t *testing.T) {
	clusterA, clusterB := uuid.New(), uuid.New()
	cpA1 := cacheServingNode(clusterA, "a-cp-1", "198.51.100.7")
	cpA2 := cacheServingNode(clusterA, "a-cp-2", "198.51.100.8")
	cpA3SameIPAsA1 := cacheServingNode(clusterA, "a-cp-3", "198.51.100.7")
	cpB1 := cacheServingNode(clusterB, "b-cp-1", "198.51.100.9")
	cpB1SameIPAsA1 := cacheServingNode(clusterB, "b-cp-2", "198.51.100.7")

	tests := []struct {
		name string
		// serving is the joined control-plane node that serves each successive join.
		serving []db.Node
		// expireBefore marks steps before which every cached entry has expired.
		expireBefore  map[int]bool
		wantRefreshes int
	}{
		{
			name:          "the same node serves twice within the TTL",
			serving:       []db.Node{cpA1, cpA1},
			wantRefreshes: 1,
		},
		{
			name:          "a second node serves, then the first again within the TTL",
			serving:       []db.Node{cpA1, cpA2, cpA1},
			wantRefreshes: 1,
		},
		{
			name:          "a new node reuses a deleted node's IP within the TTL",
			serving:       []db.Node{cpA1, cpA2, cpA3SameIPAsA1},
			wantRefreshes: 1,
		},
		{
			name:          "two clusters keep separate entries",
			serving:       []db.Node{cpA1, cpB1, cpA1, cpB1},
			wantRefreshes: 2,
		},
		{
			name:          "two clusters whose control-plane nodes share an IP",
			serving:       []db.Node{cpA1, cpB1SameIPAsA1},
			wantRefreshes: 2,
		},
		{
			name:          "an expired entry is refreshed",
			serving:       []db.Node{cpA1, cpA1},
			expireBefore:  map[int]bool{1: true},
			wantRefreshes: 2,
		},
		{
			name:          "an expired entry is refreshed through another node",
			serving:       []db.Node{cpA1, cpA2, cpA1},
			expireBefore:  map[int]bool{1: true},
			wantRefreshes: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetJoinStringCache(t)
			agent := newCertKeyRotatingAgent()
			a := joinCacheActivities(agent)

			for i, serving := range tc.serving {
				if tc.expireBefore[i] {
					expireJoinStringCache()
				}
				resp, err := a.getKubeadmJoinStrings(context.Background(), serving)
				require.NoError(t, err, "join %d via %s", i, serving.Name)
				assert.Equal(t, agent.currentKey(serving.ClusterID), certKeyFromJoinString(t, resp.GetControlplaneJoinString()),
					"join %d via %s (%s) must get the key that last encrypted its cluster's kubeadm-certs", i, serving.Name, serving.IP)
				assert.NotContains(t, resp.GetWorkerJoinString(), "--certificate-key", "join %d", i)
			}
			assert.Equal(t, tc.wantRefreshes, agent.totalRefreshes())
		})
	}
}

func TestGetKubeadmJoinStringsRefreshErrors(t *testing.T) {
	clusterID := uuid.New()
	cp := cacheServingNode(clusterID, "cp-1", "198.51.100.7")

	t.Run("an agent error is returned and nothing is cached", func(t *testing.T) {
		resetJoinStringCache(t)
		agent := newCertKeyRotatingAgent()
		agent.failNext = errors.New("agent unavailable")
		a := joinCacheActivities(agent)

		_, err := a.getKubeadmJoinStrings(context.Background(), cp)
		require.ErrorContains(t, err, "agent unavailable")

		resp, err := a.getKubeadmJoinStrings(context.Background(), cp)
		require.NoError(t, err)
		assert.Equal(t, agent.currentKey(clusterID), certKeyFromJoinString(t, resp.GetControlplaneJoinString()))
		assert.Equal(t, 1, agent.totalRefreshes())
	})

	t.Run("empty join strings are rejected and nothing is cached", func(t *testing.T) {
		resetJoinStringCache(t)
		agent := newCertKeyRotatingAgent()
		agent.emptyNext = true
		a := joinCacheActivities(agent)

		_, err := a.getKubeadmJoinStrings(context.Background(), cp)
		require.ErrorContains(t, err, "missing join strings")

		resp, err := a.getKubeadmJoinStrings(context.Background(), cp)
		require.NoError(t, err)
		assert.Equal(t, agent.currentKey(clusterID), certKeyFromJoinString(t, resp.GetControlplaneJoinString()))
	})
}

func TestGetKubeadmJoinStringsConcurrentJoinsThroughDifferentNodes(t *testing.T) {
	resetJoinStringCache(t)
	clusterID := uuid.New()
	serving := []db.Node{
		cacheServingNode(clusterID, "cp-1", "198.51.100.7"),
		cacheServingNode(clusterID, "cp-2", "198.51.100.8"),
		cacheServingNode(clusterID, "cp-3", "198.51.100.9"),
	}
	agent := newCertKeyRotatingAgent()
	a := joinCacheActivities(agent)

	const joins = 30
	joinStrings := make([]string, joins)
	errs := make([]error, joins)
	var wg sync.WaitGroup
	for i := range joins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := a.getKubeadmJoinStrings(context.Background(), serving[i%len(serving)])
			errs[i] = err
			joinStrings[i] = resp.GetControlplaneJoinString()
		}()
	}
	wg.Wait()

	for i := range joins {
		require.NoError(t, errs[i], "join %d", i)
		assert.Equal(t, agent.currentKey(clusterID), certKeyFromJoinString(t, joinStrings[i]), "join %d", i)
	}
	assert.Equal(t, 1, agent.totalRefreshes(), "parallel joins of one cluster must share one refresh")
}
