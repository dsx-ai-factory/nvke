// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type Node struct {
	ID uuid.UUID `bun:"type:uuid,pk,default:gen_random_uuid()"`
	// Transition contract (especially for control-plane nodes):
	// - control-plane nodes with non-null node_pool_id/node_set_id are managed by nodepool controller
	// - control-plane nodes with null node_pool_id/node_set_id are managed by no-nodepool control-plane controller
	// - worker nodes remain nodepool-managed with non-null node_pool_id/node_set_id
	// Today active flows still create control-plane nodes with non-null node_pool_id/node_set_id;
	// null values are reserved for the future no-nodepool control-plane path.
	NodePoolID   uuid.UUID `bun:"type:uuid,nullzero"` // Denormalized for api/join performance, also Node's will never change NodePools.
	NodeSetID    uuid.UUID `bun:"type:uuid,nullzero"`
	ClusterID    uuid.UUID `bun:"type:uuid,notnull"`
	ControlPlane bool      `bun:"type:boolean,notnull,default:false"`
	Name         string    `bun:"type:text,notnull"`
	// FailureDomain is the requested placement for this node. An empty string
	// requests no specific domain.
	FailureDomain                string                 `bun:"type:text,notnull,default:''"`
	IP                           string                 `bun:"type:text,notnull"`
	SpiffeEnabled                bool                   `bun:"type:boolean,notnull,default:false"`
	SpireAttestationMethod       SpireAttestationMethod `bun:"type:text,notnull"`
	NodeAgentExternalMTLSEnabled bool                   `bun:"node_agent_external_mtls_enabled,type:boolean,notnull,default:false"`
	NodeAgentTLSJWTEnabled       bool                   `bun:"node_agent_tls_jwt_enabled,type:boolean,notnull,default:false"`
	Conditions                   NodeConditions         `bun:"type:jsonb"`
	AgentVersion                 string                 `bun:"type:text,notnull,default:''"`
	ControlPlaneConfigurationID  uuid.UUID              `bun:"configuration_id,type:uuid,nullzero"`
	// DeleteRequest holds the TeardownNode input recorded when a delete was
	// accepted but not yet started. It is set only on the control-plane-controller
	// delete path, where the API records intent and the control-plane controller
	// starts the teardown one node at a time; nil everywhere else.
	DeleteRequest json.RawMessage   `bun:"delete_request,type:jsonb,nullzero"`
	CreatedAt     time.Time         `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt     time.Time         `bun:"type:timestamptz,notnull,default:current_timestamp"`
	Nico          *NicoNodeProvider `bun:"nico,type:jsonb"`
}

type NicoNodeProvider struct {
	ForgeInstanceID string    `json:"forge_instance_id,omitempty"`
	ForgeMachineID  string    `json:"forge_machine_id,omitempty"`
	InstanceTypeID  uuid.UUID `json:"instance_type_id,omitempty"`
}

// ValidateSecurityProfile rejects node records whose immutable connection and
// attestation settings were not selected at creation time.
func (n Node) ValidateSecurityProfile() error {
	switch n.SpireAttestationMethod {
	case SpireAttestationMethodTPM:
	case SpireAttestationMethodJoinToken, SpireAttestationMethodDSXIMDS:
		if !n.SpiffeEnabled {
			return fmt.Errorf("node %s requires SPIFFE for %s attestation", n.ID, n.SpireAttestationMethod)
		}
	default:
		return fmt.Errorf("node %s has invalid SPIRE attestation method %q", n.ID, n.SpireAttestationMethod)
	}
	if n.NodeAgentTLSJWTEnabled && !n.SpiffeEnabled && !n.NodeAgentExternalMTLSEnabled {
		return fmt.Errorf("node %s requires a TLS transport for TLS/JWT", n.ID)
	}
	return nil
}

// IsManagedByNodePool reports whether the node is managed by a node pool controller.
func (n Node) IsManagedByNodePool() bool {
	return n.NodePoolID != uuid.Nil
}

// IsManagedByControlPlaneController reports whether the node is a no-nodepool control plane node.
func (n Node) IsManagedByControlPlaneController() bool {
	return n.ControlPlane && n.NodePoolID == uuid.Nil
}

// Example: spiffe://nke-site-forge-az24-dev7.nke.nvidia.com/host/<node-id>
func (n *Node) GetSpiffeID(trustDomain string) string {
	return fmt.Sprintf("spiffe://%s/host/%s", trustDomain, n.ID.String())
}

type NodeConditions struct {
	Pending             Condition `json:"pending"`
	ProvisioningClaimed Condition `json:"provisioningClaimed"`
	Provisioning        Condition `json:"provisioning"`
	ReadyToJoin         Condition `json:"readyToJoin"`
	Joined              Condition `json:"joined"`
	Provisioned         Condition `json:"provisioned"`
	// DeleteRequested marks a delete that has been accepted but whose teardown
	// has not started yet. Deleting means the teardown itself is in flight, so at
	// most one of the two is true at a time.
	DeleteRequested         Condition `json:"deleteRequested"`
	Deleting                Condition `json:"deleting"`
	KubernetesReady         Condition `json:"kubernetesReady"`
	KubernetesUnschedulable Condition `json:"kubernetesUnschedulable"`
	NodeAgentReady          Condition `json:"nodeAgentReady"`
	Rebooting               Condition `json:"rebooting"`
	MarkedForRecycling      Condition `json:"markedForRecycling"`
	MarkedForRepair         Condition `json:"markedForRepair"`
	TeleportRegistered      Condition `json:"teleportRegistered"`
	// ControlPlaneRolloutRequired reports whether a control-plane-controller-managed
	// node's pinned control-plane configuration has drifted from the cluster's
	// desired configuration and the node therefore needs to be rolled (True), or is
	// up to date (False). Zero value (empty status) for nodes not managed by the
	// control-plane controller.
	ControlPlaneRolloutRequired Condition `json:"controlPlaneRolloutRequired"`
}

// NamedCondition pairs a condition with the name it carries in JSON.
type NamedCondition struct {
	Name      string
	Condition Condition
}

// All returns every condition with its name, in declaration order. It is the
// one list of the set's members: Equal compares against it, and callers that
// need to report which condition changed read it rather than keeping a second
// list that can drift out of step with this one.
//
// Every field must appear here; TestUnit_NodeConditionsAllCoversEveryField
// fails if one is missed.
func (c NodeConditions) All() []NamedCondition {
	return []NamedCondition{
		{"pending", c.Pending},
		{"provisioningClaimed", c.ProvisioningClaimed},
		{"provisioning", c.Provisioning},
		{"readyToJoin", c.ReadyToJoin},
		{"joined", c.Joined},
		{"provisioned", c.Provisioned},
		{"deleteRequested", c.DeleteRequested},
		{"deleting", c.Deleting},
		{"kubernetesReady", c.KubernetesReady},
		{"kubernetesUnschedulable", c.KubernetesUnschedulable},
		{"nodeAgentReady", c.NodeAgentReady},
		{"rebooting", c.Rebooting},
		{"markedForRecycling", c.MarkedForRecycling},
		{"markedForRepair", c.MarkedForRepair},
		{"teleportRegistered", c.TeleportRegistered},
		{"controlPlaneRolloutRequired", c.ControlPlaneRolloutRequired},
	}
}

// Equal reports whether two condition sets describe the same state. Use this
// instead of ==, which compares the monotonic clock reading and the *Location
// pointer of every LastTransitionTime and so reports a difference for two
// identical instants that took different routes into memory.
func (c NodeConditions) Equal(other NodeConditions) bool {
	mine, theirs := c.All(), other.All()
	for i := range mine {
		if !mine[i].Condition.Equal(theirs[i].Condition) {
			return false
		}
	}
	return true
}

type NodeConditionStatus struct {
	ClusterName     string          `bun:"cluster_name"`
	NodeName        string          `bun:"node_name"`
	ConditionType   string          `bun:"condition_type"`
	ConditionStatus ConditionStatus `bun:"condition_status"`
}

type NodeLifecycleElapsed struct {
	ClusterName string  `bun:"cluster_name"`
	NodeName    string  `bun:"node_name"`
	NodeType    string  `bun:"node_type"`
	State       string  `bun:"state"`
	ElapsedSecs float64 `bun:"elapsed_secs"`
}

type NodeInfo struct {
	ClusterName    string    `bun:"cluster_name"`
	NodePoolName   string    `bun:"node_pool_name"`
	NodeSetID      uuid.UUID `bun:"node_set_id"`
	NodeName       string    `bun:"node_name"`
	IP             string    `bun:"ip"`
	AgentVersion   string    `bun:"agent_version"`
	SpiffeEnabled  bool      `bun:"spiffe_enabled"`
	ForgeMachineID string    `bun:"forge_machine_id"`
	OSProfile      string    `bun:"os_profile"`
	ConfigProfile  string    `bun:"config_profile"`
}

func (n *Node) NicoProvider() (NicoNodeProvider, error) {
	if n.Nico == nil {
		return NicoNodeProvider{}, fmt.Errorf("%w: node %s nico", ErrMissingProviderPayload, n.ID)
	}
	return *n.Nico, nil
}

func (n *Node) SetNico(provider NicoNodeProvider) {
	n.Nico = &provider
}

// NicoInstanceTypeID returns the instance type recorded on the node's Nico
// payload, or the zero UUID when no type has been recorded yet. Control-plane
// nodes are created before the scheduler picks a type, so the zero value is a
// normal state rather than an error.
func (n *Node) NicoInstanceTypeID() uuid.UUID {
	if n.Nico == nil {
		return uuid.Nil
	}
	return n.Nico.InstanceTypeID
}

// NicoForgeInstanceID returns the Forge instance backing the node, or the empty
// string when the node has no Forge instance — a CAPI node never has one, and a
// Nico node does not until Forge has created it.
func (n *Node) NicoForgeInstanceID() string {
	if n.Nico == nil {
		return ""
	}
	return n.Nico.ForgeInstanceID
}

// NicoForgeMachineID returns the Forge machine backing the node, or the empty
// string when the node has none.
func (n *Node) NicoForgeMachineID() string {
	if n.Nico == nil {
		return ""
	}
	return n.Nico.ForgeMachineID
}

type NodeFilters struct {
	ID                 *uuid.UUID
	ClusterID          *uuid.UUID
	NodePoolID         *uuid.UUID
	NodeSetID          *uuid.UUID
	ControlPlane       *bool
	SiteID             *uuid.UUID
	ClusterName        *string
	Name               *string
	TeleportRegistered *bool

	// Condition filters use asymmetric matching:
	//   - true:  exact match (status = 'True')
	//   - false: not definitely true (includes 'False', 'Unknown', and missing/NULL)
	Pending                 *bool
	ProvisioningClaimed     *bool
	Provisioning            *bool
	Joined                  *bool
	DeleteRequested         *bool
	Deleting                *bool
	KubernetesReady         *bool
	KubernetesUnschedulable *bool
	MarkedForRecycling      *bool
	MarkedForRepair         *bool
}

//nolint:interfacebloat // Data access layer interfaces commonly have many methods
type NodeQueries interface {
	ListNodes(ctx context.Context, filters NodeFilters) ([]Node, error)
	ListNodeConditions(ctx context.Context) ([]NodeConditionStatus, error)
	ListNodeInfo(ctx context.Context) ([]NodeInfo, error)
	ListClusterNodeNames(ctx context.Context, clusterID uuid.UUID) ([]string, error)
	GetNode(ctx context.Context, id uuid.UUID) (Node, error)
	GetNodeForUpdate(ctx context.Context, id uuid.UUID) (Node, error)
	CreateNode(ctx context.Context, node *Node) (Node, error)
	UpdateNode(ctx context.Context, node *Node) (Node, error)
	DeleteNode(ctx context.Context, id uuid.UUID) error

	// TryAcquireNodeProvisioningLock tries to acquire a transaction-level advisory lock for a specific node.
	// This lock should be held during node provisioning operations to prevent race conditions
	// between the auto-provisioner and provision workflows.
	// It will return false if the lock is already held.
	TryAcquireNodeProvisioningLock(ctx context.Context, nodeID uuid.UUID) (bool, error)

	// ListNodesEligibleForRecycling returns nodes from deprecated NodeSets that haven't yet been
	// marked for recycling. Results are ordered by node creation time (oldest first) so that
	// older nodes are recycled before newer ones.
	// Note: A limit of 0 will return 0 rows.
	ListNodesEligibleForRecycling(ctx context.Context, nodePoolID uuid.UUID, limit int) ([]Node, error)

	UpdateNodeTeleportRegistered(ctx context.Context, nodeID uuid.UUID, condition Condition) error

	// ListTeleportEligibleNodes returns all nodes eligible for Teleport management at a site
	ListTeleportEligibleNodes(ctx context.Context, siteID uuid.UUID) ([]Node, error)

	ListTeleportRegisteredNodes(ctx context.Context, siteID uuid.UUID) ([]Node, error)

	// ListTeleportRolloutStats returns per-node-pool aggregates for Teleport metrics.
	ListTeleportRolloutStats(ctx context.Context) ([]TeleportRolloutRow, error)

	ListNodeLifecycleElapsed(ctx context.Context) ([]NodeLifecycleElapsed, error)
}

func (q *queriesImpl) ListNodes(ctx context.Context, filters NodeFilters) ([]Node, error) {
	var nodes []Node
	query := q.db.NewSelect().Model(&nodes).
		Join("LEFT JOIN node_sets nps ON nps.id = node.node_set_id").
		Join("LEFT JOIN node_pools np ON np.id = COALESCE(nps.node_pool_id, node.node_pool_id)").
		Join("LEFT JOIN clusters c ON c.id = COALESCE(np.cluster_id, node.cluster_id)")

	if filters.ID != nil {
		query = query.Where("node.id = ?", filters.ID)
	}
	if filters.NodePoolID != nil {
		query = query.Where("COALESCE(nps.node_pool_id, node.node_pool_id) = ?", filters.NodePoolID)
	}
	if filters.NodeSetID != nil {
		query = query.Where("node.node_set_id = ?", filters.NodeSetID)
	}
	if filters.ClusterID != nil {
		query = query.Where("node.cluster_id = ?", filters.ClusterID)
	}
	if filters.SiteID != nil {
		query = query.Where("c.nico->>'site_id' = ?", filters.SiteID.String())
	}
	if filters.ControlPlane != nil {
		query = query.Where("COALESCE(np.control_plane, node.control_plane) = ?", *filters.ControlPlane)
	}
	if filters.ClusterName != nil {
		query = query.Where("c.name = ?", *filters.ClusterName)
	}
	if filters.Name != nil {
		query = query.Where("node.name = ?", *filters.Name)
	}
	if filters.TeleportRegistered != nil {
		query = whereNodeConditionStatus(query, "teleportRegistered", *filters.TeleportRegistered)
	}
	if filters.Pending != nil {
		query = whereNodeConditionStatus(query, "pending", *filters.Pending)
	}
	if filters.ProvisioningClaimed != nil {
		query = whereNodeConditionStatus(query, "provisioningClaimed", *filters.ProvisioningClaimed)
	}
	if filters.Provisioning != nil {
		query = whereNodeConditionStatus(query, "provisioning", *filters.Provisioning)
	}
	if filters.Joined != nil {
		query = whereNodeConditionStatus(query, "joined", *filters.Joined)
	}
	if filters.DeleteRequested != nil {
		query = whereNodeConditionStatus(query, "deleteRequested", *filters.DeleteRequested)
	}
	if filters.Deleting != nil {
		query = whereNodeConditionStatus(query, "deleting", *filters.Deleting)
	}
	if filters.KubernetesReady != nil {
		query = whereNodeConditionStatus(query, "kubernetesReady", *filters.KubernetesReady)
	}
	if filters.KubernetesUnschedulable != nil {
		query = whereNodeConditionStatus(query, "kubernetesUnschedulable", *filters.KubernetesUnschedulable)
	}
	if filters.MarkedForRecycling != nil {
		query = whereNodeConditionStatus(query, "markedForRecycling", *filters.MarkedForRecycling)
	}
	if filters.MarkedForRepair != nil {
		query = whereNodeConditionStatus(query, "markedForRepair", *filters.MarkedForRepair)
	}

	err := query.Scan(ctx)
	return nodes, err
}

// whereNodeConditionStatus applies a node condition filter to the query.
// When filtering for true, uses exact match (status = 'True').
// When filtering for false, uses NOT true (includes False, Unknown, and NULL/missing).
func whereNodeConditionStatus(query *bun.SelectQuery, conditionName string, value bool) *bun.SelectQuery {
	jsonPath := fmt.Sprintf("node.conditions->'%s'->>'status'", conditionName)
	if value {
		return query.Where(jsonPath+" = ?", ConditionStatusTrue)
	}
	return query.Where("COALESCE("+jsonPath+", '') != ?", ConditionStatusTrue)
}

// ListNodeConditions returns a list of all node conditions, for metrics purposes.
func (q *queriesImpl) ListNodeConditions(ctx context.Context) ([]NodeConditionStatus, error) {
	var results []NodeConditionStatus
	err := q.db.NewRaw(`
		select
			c.name as cluster_name,
			n.name as node_name,
			key as condition_type,
			value->>'status' as condition_status
		from
			nodes n
			join node_pools np on n.node_pool_id = np.id
			join clusters c on c.id = np.cluster_id
			cross join jsonb_each(n.conditions)
	`).Scan(ctx, &results)
	if err != nil {
		return nil, err
	}

	return results, nil
}

func (q *queriesImpl) ListNodeInfo(ctx context.Context) ([]NodeInfo, error) {
	var results []NodeInfo
	err := q.db.NewRaw(`
		SELECT 
			c.name as cluster_name,
			np.name as node_pool_name,
			ns.id as node_set_id,
			n.name as node_name,
			n.ip,
			n.agent_version,
			n.spiffe_enabled,
			COALESCE(n.nico->>'forge_machine_id', '') as forge_machine_id,
			COALESCE(ns.nico->>'os_profile', '') as os_profile,
			COALESCE(ns.nico->>'config_profile', '') as config_profile
		FROM nodes n
		JOIN node_sets ns ON ns.id = n.node_set_id
		JOIN node_pools np ON np.id = ns.node_pool_id
		JOIN clusters c ON c.id = np.cluster_id
		ORDER BY c.name, np.name, n.name
	`).Scan(ctx, &results)
	if err != nil {
		return nil, err
	}

	return results, nil
}

func (q *queriesImpl) ListClusterNodeNames(ctx context.Context, clusterID uuid.UUID) ([]string, error) {
	var names []string
	err := q.db.NewSelect().Model(&Node{}).
		ColumnExpr("node.name").
		Where("node.cluster_id = ?", clusterID).
		Scan(ctx, &names)

	return names, err
}

func (q *queriesImpl) GetNode(ctx context.Context, id uuid.UUID) (Node, error) {
	var node Node
	err := q.db.NewSelect().Model(&node).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	return node, nil
}

func (q *queriesImpl) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (Node, error) {
	var node Node
	err := q.db.NewSelect().
		Model(&node).
		Where("id = ?", id).
		For("UPDATE").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	return node, nil
}

func (q *queriesImpl) CreateNode(ctx context.Context, node *Node) (Node, error) {
	if err := node.ValidateSecurityProfile(); err != nil {
		return Node{}, err
	}
	_, err := q.db.NewInsert().Model(node).Exec(ctx)
	return *node, err
}

func (q *queriesImpl) UpdateNode(ctx context.Context, node *Node) (Node, error) {
	node.UpdatedAt = time.Now()
	res, err := q.db.NewUpdate().Model(node).
		ExcludeColumn("spiffe_enabled", "spire_attestation_method", "node_agent_external_mtls_enabled", "node_agent_tls_jwt_enabled").
		WherePK().Returning("id, spiffe_enabled, spire_attestation_method, node_agent_external_mtls_enabled, node_agent_tls_jwt_enabled").Exec(ctx)
	if err != nil {
		return Node{}, err
	}
	return *node, notFoundErrIfNoRowsAffected(res)
}

func (q *queriesImpl) DeleteNode(ctx context.Context, id uuid.UUID) error {
	_, err := q.db.NewDelete().Model(&Node{}).Where("id = ?", id).Exec(ctx)
	return err
}

func (q *queriesImpl) UpdateNodeTeleportRegistered(ctx context.Context, nodeID uuid.UUID, condition Condition) error {
	return q.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		qi := queriesImpl{db: tx}
		node, err := qi.GetNodeForUpdate(ctx, nodeID)
		if err != nil {
			return err
		}
		node.Conditions.TeleportRegistered = condition
		_, err = qi.UpdateNode(ctx, &node)
		return err
	})
}

func (q *queriesImpl) ListTeleportEligibleNodes(ctx context.Context, siteID uuid.UUID) ([]Node, error) {
	var nodes []Node
	query := q.db.NewSelect().Model(&nodes).
		Join("LEFT JOIN node_sets ns ON ns.id = node.node_set_id").
		Join("LEFT JOIN node_pools np ON np.id = COALESCE(ns.node_pool_id, node.node_pool_id)").
		Join("LEFT JOIN clusters c ON c.id = COALESCE(np.cluster_id, node.cluster_id)").
		Where("c.nico->>'site_id' = ?", siteID.String()).
		Where("c.teleport_enabled = true").
		Where(`(
			COALESCE(ns.teleport_enabled, false) = true
			OR (node.control_plane = true AND node.node_set_id IS NULL)
		)`)

	query = whereNodeConditionStatus(query, "deleting", false)

	err := query.Scan(ctx)
	return nodes, err
}

func (q *queriesImpl) ListTeleportRegisteredNodes(ctx context.Context, siteID uuid.UUID) ([]Node, error) {
	var nodes []Node
	query := q.db.NewSelect().Model(&nodes).
		Join("JOIN node_sets ns ON ns.id = node.node_set_id").
		Join("JOIN node_pools np ON np.id = ns.node_pool_id").
		Join("JOIN clusters c ON c.id = np.cluster_id").
		Where("c.nico->>'site_id' = ?", siteID.String()).
		Where("c.teleport_enabled = true").
		Where("ns.teleport_enabled = true")
	query = whereNodeConditionStatus(query, "teleportRegistered", true)

	query = whereNodeConditionStatus(query, "deleting", false)

	err := query.Scan(ctx)
	return nodes, err
}

// nodeProvisioningLockKey generates a consistent lock key for a node.
// We use a hash of the UUID to convert it to an int64 for PostgreSQL advisory locks.
func nodeProvisioningLockKey(nodeID uuid.UUID) int64 {
	h := fnv.New64a()
	h.Write(nodeID[:])
	return int64(h.Sum64()) //nolint:gosec // Intentional overflow to use full int64 range for advisory locks
}

func (q *queriesImpl) TryAcquireNodeProvisioningLock(ctx context.Context, nodeID uuid.UUID) (bool, error) {
	lockKey := nodeProvisioningLockKey(nodeID)

	// Use pg_try_advisory_xact_lock which returns true if the lock was acquired, false if it was not.
	// It automatically releases the lock at transaction end.
	var acquired bool
	err := q.db.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock(?)", lockKey).Scan(&acquired)
	if err != nil {
		return false, fmt.Errorf("failed to try to acquire advisory lock for node %s: %w", nodeID, err)
	}

	return acquired, nil
}

// ListNodesEligibleForRecycling returns nodes from deprecated NodeSets that haven't yet been
// marked for recycling. Results are ordered by node creation time (oldest first) so that
// older nodes are recycled before newer ones.
// Note: A limit of 0 will return 0 rows.
func (q *queriesImpl) ListNodesEligibleForRecycling(ctx context.Context, nodePoolID uuid.UUID, limit int) ([]Node, error) {
	if limit <= 0 {
		// bun treats Limit(0) as no limit, most people find this surprising and non-idiomatic
		// this guard provides the expected SQL LIMIT 0 type behavior.
		return nil, nil
	}

	var nodes []Node
	query := q.db.NewSelect().Model(&nodes).
		Join("JOIN node_sets ns ON ns.id = node.node_set_id").
		Join("JOIN node_pools np ON np.id = ns.node_pool_id").
		Where("ns.node_pool_id = ?", nodePoolID).
		Where("ns.id != np.current_node_set_id").
		Order("node.created_at ASC").
		Limit(limit)
	query = whereNodeConditionStatus(query, "markedForRecycling", false)

	err := query.Scan(ctx)
	return nodes, err
}

func (q *queriesImpl) ListNodeLifecycleElapsed(ctx context.Context) ([]NodeLifecycleElapsed, error) {
	var results []NodeLifecycleElapsed
	err := q.db.NewRaw(`
		SELECT
			c.name AS cluster_name,
			n.name AS node_name,
			CASE WHEN np.control_plane THEN 'cp' ELSE 'worker' END AS node_type,
			CASE
				WHEN n.conditions->'deleting'->>'status' = 'True'
					THEN 'deleting'
				WHEN n.conditions->'provisioning'->>'status' = 'True'
					THEN 'provisioning'
				WHEN n.conditions->'pending'->>'status' = 'True'
					THEN 'pending'
			END AS state,
			EXTRACT(EPOCH FROM (NOW() - (
				CASE
					WHEN n.conditions->'deleting'->>'status' = 'True'
						THEN (n.conditions->'deleting'->>'lastTransitionTime')::timestamptz
					WHEN n.conditions->'provisioning'->>'status' = 'True'
						THEN (n.conditions->'provisioning'->>'lastTransitionTime')::timestamptz
					WHEN n.conditions->'pending'->>'status' = 'True'
						THEN (n.conditions->'pending'->>'lastTransitionTime')::timestamptz
				END
			))) AS elapsed_secs
		FROM nodes n
		JOIN node_sets ns ON ns.id = n.node_set_id
		JOIN node_pools np ON np.id = ns.node_pool_id
		JOIN clusters c ON c.id = np.cluster_id
		WHERE
			(n.conditions->'deleting'->>'status' = 'True' AND n.conditions->'deleting'->>'lastTransitionTime' IS NOT NULL)
			OR (n.conditions->'provisioning'->>'status' = 'True' AND n.conditions->'provisioning'->>'lastTransitionTime' IS NOT NULL)
			OR (n.conditions->'pending'->>'status' = 'True' AND n.conditions->'pending'->>'lastTransitionTime' IS NOT NULL)
		UNION ALL
		-- markedForRecycling can overlap with provisioned/etc., so it is emitted as a separate row
		-- rather than folded into the CASE above.
		SELECT
			c.name AS cluster_name,
			n.name AS node_name,
			CASE WHEN np.control_plane THEN 'cp' ELSE 'worker' END AS node_type,
			'markedForRecycling' AS state,
			EXTRACT(EPOCH FROM (NOW() - (n.conditions->'markedForRecycling'->>'lastTransitionTime')::timestamptz)) AS elapsed_secs
		FROM nodes n
		JOIN node_sets ns ON ns.id = n.node_set_id
		JOIN node_pools np ON np.id = ns.node_pool_id
		JOIN clusters c ON c.id = np.cluster_id
		WHERE n.conditions->'markedForRecycling'->>'status' = 'True'
			AND n.conditions->'markedForRecycling'->>'lastTransitionTime' IS NOT NULL
	`).Scan(ctx, &results)
	if err != nil {
		return nil, err
	}

	return results, nil
}
