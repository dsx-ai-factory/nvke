// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodecontract"
	"github.com/dsx-ai-factory/nvke/controller/internal/spiffe"
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
	credentialAudit          *credentialaudit.Recorder
	// certificateRenewalReleaseBackoff overrides the first release retry wait
	// when non-zero, so tests need not wait the real backoff.
	certificateRenewalReleaseBackoff time.Duration
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
		credentialAudit:          credentialaudit.NewRecorder(database, nil, metrics.RecordCredentialAuditWrite),
	}
}

func (a *Activities) nodeAgentBootstrapFiles(node db.Node) ([]db.CloudInitFile, error) {
	contracts := nodecontract.ForAgentVersion(node.AgentVersion)
	var files []db.CloudInitFile
	args := []string{"--secure-server-port", a.cfg.NodeAgent.Port}
	if node.NodeAgentExternalMTLSEnabled {
		if a.controllerClientCABundle == "" {
			return nil, errors.New("controller client CA bundle is required when external node-agent mTLS is enabled")
		}

		files = append(files, db.CloudInitFile{
			Path:        contracts.ControllerCABundlePath,
			Owner:       "root:root",
			Permissions: "0644",
			Encoding:    "b64",
			Content:     base64.StdEncoding.EncodeToString([]byte(a.controllerClientCABundle)),
		})
		args = append(args, "--nvke-controller-client-ca-bundle="+contracts.ControllerCABundlePath)
	}
	if node.NodeAgentTLSJWTEnabled {
		args = append(args, "--grpc-auth-mode=tls-jwt", "--dsx-identity-exchange-url="+a.cfg.NodeAgent.TLSJWT.DSXIdentityExchangeURL)
		// Only a new-form agent understands the flag, so an empty audience means
		// send none and let the agent keep requesting the legacy one.
		if contracts.ControllerAudience != "" {
			args = append(args, "--controller-audience="+contracts.ControllerAudience)
		}
	}
	// The controller dials such a node in plaintext, so the agent must serve plaintext too.
	if !node.SpiffeEnabled && !node.NodeAgentExternalMTLSEnabled {
		args = append(args, "--disable-mtls")
	}
	files = append(files, db.CloudInitFile{
		Path:        contracts.EnvFilePath,
		Owner:       "root:root",
		Permissions: "0644",
		Content:     fmt.Sprintf("AGENT_ARGS=%q\n", strings.Join(args, " ")),
	})

	return files, nil
}
