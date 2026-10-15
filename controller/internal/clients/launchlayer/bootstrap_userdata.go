// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// Adapted from capi/capi_spire_agent.conf.tmpl, with the join_token stanza
// and its NodeAttestor removed: CAPLL is the production-target provider,
// and join-token attestation must not be reachable for it even as a
// configurable option. CreateMachine rejects a node configured for it
// before this template is ever rendered.
//
//go:embed launchlayer_spire_agent.conf.tmpl
var spireAgentConfigTemplateText string

type cloudInitConfig struct {
	WriteFiles []cloudInitFile `yaml:"write_files,omitempty"`
	RunCmd     []string        `yaml:"runcmd,omitempty"`
}

type cloudInitFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner,omitempty"`
	Permissions string `yaml:"permissions,omitempty"`
	Encoding    string `yaml:"encoding,omitempty"`
	Content     string `yaml:"content"`
}

func (f cloudInitFile) MarshalYAML() (interface{}, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	appendMappingNode(node, "path", 0, f.Path, "")
	if f.Owner != "" {
		appendMappingNode(node, "owner", 0, f.Owner, "")
	}
	if f.Permissions != "" {
		appendMappingNode(node, "permissions", yaml.DoubleQuotedStyle, f.Permissions, "!!str")
	}
	if f.Encoding != "" {
		appendMappingNode(node, "encoding", 0, f.Encoding, "")
	}
	contentStyle := yaml.LiteralStyle
	if f.Encoding != "" {
		contentStyle = 0
	}
	appendMappingNode(node, "content", contentStyle, f.Content, "")
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

// renderBootstrapUserData renders a LaunchLayer node's cloud-init user data.
// CAPLL passes the complete rendered bootstrap Secret value straight through
// as provisioning.profile_args.user_data (see CAPLL's docs/profile-args.md)
// — it does not template or extract values itself, so everything the node
// needs (controller files, SPIRE bootstrap material) must already be in
// this payload.
func (*Client) renderBootstrapUserData(
	ctx context.Context,
	deps shared.MachineCreateDeps,
	node db.Node,
) (string, error) {
	controllerFiles, err := deps.NodeAgentBootstrapFiles(node)
	if err != nil {
		return "", err
	}

	// A site stood up without SPIFFE has no SPIRE and no workload socket to
	// read a bundle from, so a node gets neither the bundle nor a SPIRE agent
	// config. Asking anyway blocks provisioning: the dial fails, the activity
	// retries forever, and the cluster never leaves Provisioning.
	var trustDomain, trustBundle string
	if node.SpiffeEnabled {
		trustDomain, trustBundle, err = deps.GetTrustDomainBundle(ctx, deps.Config)
		if err != nil {
			return "", fmt.Errorf("failed to get trust domain and bundle: %w", err)
		}
	}

	files := make([]cloudInitFile, 0, len(controllerFiles)+2)
	if node.SpiffeEnabled {
		dsxIMDSPluginChecksum := deps.Config.NodeAgent.DSXImds.PluginChecksum
		if dsxIMDSPluginChecksum == "" {
			dsxIMDSPluginChecksum = "${DSX_IMDS_PLUGIN_CHECKSUM}"
		}
		files = append(files,
			cloudInitFile{
				Path:        constants.CAPISpireTrustBundlePath,
				Owner:       "root:root",
				Permissions: "0644",
				Content:     trustBundle,
			},
			cloudInitFile{
				Path:        constants.CAPISpireAgentConfigPath,
				Owner:       "root:root",
				Permissions: "0644",
				Content: buildSpireAgentConfig(spireAgentConfig{
					AttestationMethod:     string(node.SpireAttestationMethod),
					ServerAddress:         deps.Config.SpireServerAddress,
					ServerPort:            deps.Config.SpireServerPort,
					TrustDomain:           trustDomain,
					TrustBundlePath:       constants.CAPISpireTrustBundlePath,
					DSXIMDSEndpoint:       deps.Config.NodeAgent.DSXImds.Endpoint,
					DSXIMDSPluginChecksum: dsxIMDSPluginChecksum,
				}),
			},
		)
	}

	for _, file := range controllerFiles {
		files = append(files, cloudInitFile{
			Path:        file.Path,
			Owner:       file.Owner,
			Permissions: file.Permissions,
			Encoding:    file.Encoding,
			Content:     file.Content,
		})
	}

	userData := cloudInitConfig{
		WriteFiles: files,
		RunCmd:     []string{"systemctl restart nke-agent.service"},
	}

	yamlBytes, err := yaml.Marshal(userData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal LaunchLayer bootstrap cloud-init: %w", err)
	}

	return "#cloud-config\n" + string(yamlBytes), nil
}

type spireAgentConfig struct {
	ServerAddress         string
	ServerPort            string
	TrustDomain           string
	TrustBundlePath       string
	AttestationMethod     string
	DSXIMDSEndpoint       string
	DSXIMDSPluginChecksum string
}

func buildSpireAgentConfig(data spireAgentConfig) string {
	var buf bytes.Buffer
	tmpl := template.Must(template.New("launchlayer-spire-agent-config").Parse(spireAgentConfigTemplateText))
	if err := tmpl.Execute(&buf, data); err != nil {
		panic(fmt.Sprintf("failed to render LaunchLayer SPIRE agent config template: %v", err))
	}

	return buf.String()
}
