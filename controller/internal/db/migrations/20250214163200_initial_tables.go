// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewCreateTable().
			Model((*Cluster20250214163200)(nil)).
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return err
		}

		_, err = db.NewCreateTable().
			Model((*NodePool20250214163200)(nil)).
			IfNotExists().
			ForeignKey(`("cluster_id") REFERENCES clusters ("id")`).
			Exec(ctx)
		if err != nil {
			return err
		}

		_, err = db.NewCreateTable().
			Model((*Node20250214163200)(nil)).
			IfNotExists().
			ForeignKey(`("node_pool_id") REFERENCES node_pools ("id")`).
			Exec(ctx)
		if err != nil {
			return err
		}

		_, err = db.NewCreateTable().
			Model((*WorkflowOutbox20250214163200)(nil)).
			IfNotExists().
			Exec(ctx)
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewDropTable().
			Model((*WorkflowOutbox20250214163200)(nil)).
			IfExists().
			Exec(ctx)
		if err != nil {
			return err
		}

		_, err = db.NewDropTable().
			Model((*Node20250214163200)(nil)).
			IfExists().
			Exec(ctx)
		if err != nil {
			return err
		}

		_, err = db.NewDropTable().
			Model((*NodePool20250214163200)(nil)).
			IfExists().
			Exec(ctx)
		if err != nil {
			return err
		}

		_, err = db.NewDropTable().
			Model((*Cluster20250214163200)(nil)).
			IfExists().
			Exec(ctx)
		return err
	})
}

type Cluster20250214163200 struct {
	bun.BaseModel `bun:"table:clusters"`

	ID                uuid.UUID                       `bun:"type:uuid,pk,default:gen_random_uuid()"`
	Name              string                          `bun:"type:text,notnull"`
	OrgID             string                          `bun:"type:text,notnull"`
	SiteID            uuid.UUID                       `bun:"type:uuid,notnull"`
	VpcID             uuid.UUID                       `bun:"type:uuid,notnull"`
	CniPlugin         string                          `bun:"type:text"`
	KubernetesVersion string                          `bun:"type:text,notnull"`
	ControlPlaneVIP   string                          `bun:"type:text,notnull"`
	Conditions        ClusterConditions20250214163200 `bun:"type:jsonb"`
	CreatedAt         time.Time                       `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt         time.Time                       `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type ClusterConditions20250214163200 struct {
	Provisioning Condition20250214163200 `json:"provisioning"`
	Provisioned  Condition20250214163200 `json:"provisioned"`
	Deleting     Condition20250214163200 `json:"deleting"`
}

type Condition20250214163200 struct {
	Status             string    `json:"status"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

type WorkflowOutbox20250214163200 struct {
	bun.BaseModel `bun:"table:workflow_outbox"`

	ID            uuid.UUID       `bun:"type:uuid,pk,default:gen_random_uuid()"`
	WorkflowType  string          `bun:"type:text,notnull"`
	WorkflowID    string          `bun:"type:text,notnull"`
	WorkflowInput json.RawMessage `bun:"type:jsonb,notnull"`
	CreatedAt     time.Time       `bun:"type:timestamptz,notnull,default:current_timestamp"`
	ProcessedAt   *time.Time      `bun:"type:timestamptz"`
}

type NodePool20250214163200 struct {
	bun.BaseModel `bun:"table:node_pools"`

	ID             uuid.UUID                        `bun:"type:uuid,pk,default:gen_random_uuid()"`
	ClusterID      uuid.UUID                        `bun:"type:uuid,notnull"`
	Name           string                           `bun:"type:text,notnull"`
	NodeNamePrefix string                           `bun:"type:text,notnull"`
	SubnetID       uuid.UUID                        `bun:"type:uuid,notnull"`
	InstanceTypeID uuid.UUID                        `bun:"type:uuid,notnull"`
	SSHKeyGroupIDs []uuid.UUID                      `bun:"type:uuid[],notnull"`
	OSProfile      string                           `bun:"type:text,notnull"`
	ConfigProfile  string                           `bun:"type:text,notnull"`
	ControlPlane   bool                             `bun:"type:boolean,notnull"`
	NodeLabels     []string                         `bun:"type:text[],notnull"`
	Conditions     NodePoolConditions20250214163200 `bun:"type:jsonb"`
	CreatedAt      time.Time                        `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt      time.Time                        `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type NodePoolConditions20250214163200 struct {
	Provisioning Condition20250214163200 `json:"provisioning"`
	Provisioned  Condition20250214163200 `json:"provisioned"`
	Deleting     Condition20250214163200 `json:"deleting"`
}

type Node20250214163200 struct {
	bun.BaseModel `bun:"table:nodes"`

	ID              uuid.UUID                    `bun:"type:uuid,pk,default:gen_random_uuid()"`
	NodePoolID      uuid.UUID                    `bun:"type:uuid,notnull"`
	Name            string                       `bun:"type:text,notnull"`
	ForgeInstanceID string                       `bun:"type:text,notnull"`
	ForgeMachineID  string                       `bun:"type:text,notnull"`
	IP              string                       `bun:"type:text,notnull"`
	Conditions      NodeConditions20250214163200 `bun:"type:jsonb"`
	CreatedAt       time.Time                    `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt       time.Time                    `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type NodeConditions20250214163200 struct {
	Provisioning Condition20250214163200 `json:"provisioning"`
	Provisioned  Condition20250214163200 `json:"provisioned"`
	Deleting     Condition20250214163200 `json:"deleting"`
}
