// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// configureTLSJWTGrpcAuthCmd migrates an existing node-agent environment file to TLS/JWT authentication:
	//   - sh -e stops immediately if any migration step fails, preventing a restart with partial configuration.
	//   - test verifies that the package-created /etc/default/nke-agent file is present before editing it.
	//   - sed removes the incompatible --disable-mtls flag and any existing --grpc-auth-mode value, then prepends
	//     --grpc-auth-mode=tls-jwt to AGENT_ARGS. The .bak suffix makes in-place editing portable across GNU and BSD sed.
	//   - rm removes the temporary backup only after sed succeeds.
	//   - grep verifies the required mode was written before systemctl restarts the agent.
	configureTLSJWTGrpcAuthCmd = `sh -ec 'agent_env=/etc/default/nke-agent; test -f "$agent_env"; sed -E -i.bak "s/--disable-mtls *//g; s/--grpc-auth-mode=[^ \" ]+ *//g; s|^AGENT_ARGS=\"|AGENT_ARGS=\"--grpc-auth-mode=tls-jwt |" "$agent_env"; rm -f "$agent_env.bak"; grep -q -- "--grpc-auth-mode=tls-jwt" "$agent_env"; systemctl restart nke-agent.service'`
	yamlStringTag              = "!!str"
)

func (a *Activities) nodeAgentTLSJWTBootstrapCommands() []string {
	if !a.cfg.NodeAgentTLSJWTEnabled {
		return nil
	}
	return []string{configureTLSJWTGrpcAuthCmd}
}

func injectNodeAgentTLSJWTBootstrap(
	userData string,
	commands []string,
) (string, error) {
	if len(commands) == 0 {
		return userData, nil
	}

	var document yaml.Node
	if err := yaml.Unmarshal([]byte(userData), &document); err != nil {
		return "", fmt.Errorf("parse rendered user data for TLS/JWT bootstrap: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("rendered user data must contain one YAML mapping")
	}

	target := document.Content[0]
	if autoinstall := mappingValue(target, "autoinstall"); autoinstall != nil {
		if autoinstall.Kind != yaml.MappingNode {
			return "", errors.New("rendered user data autoinstall must be a mapping")
		}
		target = ensureMappingValue(autoinstall, "user-data")
	}
	if target.Kind != yaml.MappingNode {
		return "", errors.New("rendered user data cloud-init target must be a mapping")
	}

	runCommands, err := ensureSequenceValue(target, "runcmd")
	if err != nil {
		return "", err
	}
	for _, command := range commands {
		runCommands.Content = append(runCommands.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   yamlStringTag,
			Value: command,
		})
	}

	var rendered bytes.Buffer
	encoder := yaml.NewEncoder(&rendered)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return "", fmt.Errorf("encode rendered user data with TLS/JWT bootstrap: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf("close rendered user data encoder: %w", err)
	}

	result := rendered.String()
	if strings.HasPrefix(strings.TrimSpace(userData), "#cloud-config") && !strings.HasPrefix(strings.TrimSpace(result), "#cloud-config") {
		result = "#cloud-config\n" + result
	}
	return result, nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func ensureMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if value := mappingValue(mapping, key); value != nil {
		return value
	}
	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStringTag, Value: key},
		value,
	)
	return value
}

func ensureSequenceValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if value := mappingValue(mapping, key); value != nil {
		if value.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("rendered user data %s must be a sequence", key)
		}
		return value, nil
	}
	value := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStringTag, Value: key},
		value,
	)
	return value, nil
}
