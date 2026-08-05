// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

//go:embed capi_spire_agent.conf.tmpl
var capiSpireAgentConfigTemplateText string

type capiCloudInitConfig struct {
	WriteFiles []capiCloudInitFile `yaml:"write_files,omitempty"`
	RunCmd     []string            `yaml:"runcmd,omitempty"`
}

type capiCloudInitFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner,omitempty"`
	Permissions string `yaml:"permissions,omitempty"`
	Content     string `yaml:"content"`
}

func (f capiCloudInitFile) MarshalYAML() (interface{}, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	appendMappingNode(node, "path", 0, f.Path, "")
	if f.Owner != "" {
		appendMappingNode(node, "owner", 0, f.Owner, "")
	}
	if f.Permissions != "" {
		appendMappingNode(node, "permissions", yaml.DoubleQuotedStyle, f.Permissions, "!!str")
	}
	appendMappingNode(node, "content", yaml.LiteralStyle, f.Content, "")
	return node, nil
}

func appendMappingNode(node *yaml.Node, key string, style yaml.Style, value, tag string) {
	valueNode := &yaml.Node{Kind: yaml.ScalarNode, Style: style, Value: value}
	if tag != "" {
		valueNode.Tag = tag
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		valueNode,
	)
}

func (a *Activities) renderCAPIBootstrapUserData(
	ctx context.Context,
	nodeSet db.NodeSet,
	spireJoinToken string,
) (string, error) {
	if nodeSet.CapiSpireAttestationMethod == db.SpireAttestationMethodJoinToken && spireJoinToken == "" {
		return "", fmt.Errorf("spire join token is required when capiSpireAttestationMethod is joinToken")
	}

	trustDomain, trustBundle, err := getTrustDomainAndBundle(ctx, a.cfg)
	if err != nil {
		return "", fmt.Errorf("failed to get trust domain and bundle: %w", err)
	}

	files := make([]capiCloudInitFile, 0, len(nodeSet.CapiBootstrapFiles)+2)
	for _, file := range nodeSet.CapiBootstrapFiles {
		files = append(files, capiCloudInitFile{
			Path:        file.Path,
			Owner:       file.Owner,
			Permissions: file.Permissions,
			Content:     file.Content,
		})
	}
	files = append(files,
		capiCloudInitFile{
			Path:        constants.CAPISpireTrustBundlePath,
			Owner:       "root:root",
			Permissions: "0644",
			Content:     trustBundle,
		},
		capiCloudInitFile{
			Path:        constants.CAPISpireAgentConfigPath,
			Owner:       "root:root",
			Permissions: "0644",
			Content: buildCAPISpireAgentConfig(
				nodeSet.CapiSpireAttestationMethod,
				a.cfg.SpireServerAddress,
				a.cfg.SpireServerPort,
				trustDomain,
				spireJoinToken,
			),
		},
	)

	userData := capiCloudInitConfig{
		WriteFiles: files,
		RunCmd:     append([]string(nil), nodeSet.CapiPreKubeadmCommands...),
	}

	yamlBytes, err := yaml.Marshal(userData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal CAPI bootstrap cloud-init: %w", err)
	}

	return "#cloud-config\n" + string(yamlBytes), nil
}

func buildCAPISpireAgentConfig(
	attestationMethod db.SpireAttestationMethod,
	serverAddress, serverPort, trustDomain, spireJoinToken string,
) string {
	data := struct {
		ServerAddress   string
		ServerPort      string
		TrustDomain     string
		TrustBundlePath string
		SpireJoinToken  string
		UseJoinToken    bool
	}{
		ServerAddress:   serverAddress,
		ServerPort:      serverPort,
		TrustDomain:     trustDomain,
		TrustBundlePath: constants.CAPISpireTrustBundlePath,
		SpireJoinToken:  spireJoinToken,
		UseJoinToken:    attestationMethod == db.SpireAttestationMethodJoinToken,
	}

	var buf bytes.Buffer
	tmpl := template.Must(template.New("capi-spire-agent-config").Parse(capiSpireAgentConfigTemplateText))
	if err := tmpl.Execute(&buf, data); err != nil {
		panic(fmt.Sprintf("failed to render CAPI SPIRE agent config template: %v", err))
	}

	return buf.String()
}
