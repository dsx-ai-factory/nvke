// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/agent"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/spiffe"
)

type trustDomainAndBundleGetter func(context.Context, config.Config) (string, string, error)

type Activities struct {
	cfg                      config.Config
	db                       db.DB
	k8sClient                ctrlclient.Client
	nodeAgentClient          agent.Client
	tenantClientProvider     TenantClientProvider
	trustDomain              string
	controllerClientCABundle string
	getTrustDomainBundle     trustDomainAndBundleGetter
}

func NewActivities(
	cfg config.Config,
	database db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
	trustDomain string,
	tenantClientProvider TenantClientProvider,
	controllerClientCABundle string,
) *Activities {
	return &Activities{
		cfg:                      cfg,
		db:                       database,
		k8sClient:                k8sClient,
		nodeAgentClient:          nodeAgentClient,
		tenantClientProvider:     tenantClientProvider,
		trustDomain:              trustDomain,
		controllerClientCABundle: controllerClientCABundle,
		getTrustDomainBundle:     spiffe.GetTrustDomainAndBundle,
	}
}

func (a *Activities) nodeAgentBootstrapFiles() ([]db.CloudInitFile, error) {
	var files []db.CloudInitFile
	args := []string{"--secure-server-port", a.cfg.NodeAgent.Port}
	if a.cfg.NodeAgentExternalMTLSEnabled {
		if a.controllerClientCABundle == "" {
			return nil, errors.New("controller client CA bundle is required when external node-agent mTLS is enabled")
		}

		files = append(files, db.CloudInitFile{
			Path:        config.NodeAgentControllerCAPath,
			Owner:       "root:root",
			Permissions: "0644",
			Encoding:    "b64",
			Content:     base64.StdEncoding.EncodeToString([]byte(a.controllerClientCABundle)),
		})
		args = append(args, "--controller-client-ca-bundle="+config.NodeAgentControllerCAPath)
	}
	if a.cfg.NodeAgent.TLSJWT.Enabled {
		args = append(args, "--grpc-auth-mode=tls-jwt", "--dsx-identity-exchange-url="+a.cfg.NodeAgent.TLSJWT.DSXIdentityExchangeURL)
	}
	files = append(files, db.CloudInitFile{
		Path:        config.NodeAgentConfigPath,
		Owner:       "root:root",
		Permissions: "0644",
		Content:     fmt.Sprintf("AGENT_ARGS=%q\n", strings.Join(args, " ")),
	})

	return files, nil
}
