// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
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
	Encoding    string `yaml:"encoding,omitempty"`
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

func (a *Activities) renderCAPIBootstrapUserData(
	ctx context.Context,
	nodeSet db.NodeSet,
	spireJoinToken string,
) (string, error) {
	capiNodeSetProvider, err := nodeSet.CAPIProvider()
	if err != nil {
		return "", fmt.Errorf("failed to read node set capi provider: %w", err)
	}
	controllerFiles, err := a.nodeAgentBootstrapFiles()
	if err != nil {
		return "", err
	}
	if a.cfg.NodeAgentSpireAttestationMethod == config.SpireAttestationMethodJoinToken && spireJoinToken == "" {
		return "", fmt.Errorf("spire join token is required when SPIRE attestation method is joinToken")
	}

	// A site stood up without SPIFFE has no SPIRE and no workload socket to
	// read a bundle from, so a node gets neither the bundle nor a SPIRE agent
	// config. Asking anyway blocks provisioning: the dial fails, the activity
	// retries forever, and the cluster never leaves Provisioning.
	var trustDomain, trustBundle string
	if a.cfg.SpiffeEnabled {
		trustDomain, trustBundle, err = a.getTrustDomainBundle(ctx, a.cfg)
		if err != nil {
			return "", fmt.Errorf("failed to get trust domain and bundle: %w", err)
		}
	}

	files := make([]capiCloudInitFile, 0, len(capiNodeSetProvider.BootstrapFiles)+len(controllerFiles)+2)
	for _, file := range capiNodeSetProvider.BootstrapFiles {
		files = append(files, capiCloudInitFile{
			Path:        file.Path,
			Owner:       file.Owner,
			Permissions: file.Permissions,
			Content:     file.Content,
		})
	}
	if a.cfg.SpiffeEnabled {
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
					db.SpireAttestationMethod(a.cfg.NodeAgentSpireAttestationMethod),
					a.cfg.SpireServerAddress,
					a.cfg.SpireServerPort,
					trustDomain,
					spireJoinToken,
				),
			},
		)
	}

	for _, file := range controllerFiles {
		files = append(files, capiCloudInitFile{
			Path:        file.Path,
			Owner:       file.Owner,
			Permissions: file.Permissions,
			Encoding:    file.Encoding,
			Content:     file.Content,
		})
	}

	runCommands := append([]string(nil), capiNodeSetProvider.PreKubeadmCommands...)
	runCommands = append(runCommands, "systemctl restart nke-agent.service")

	userData := capiCloudInitConfig{
		WriteFiles: files,
		RunCmd:     runCommands,
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
