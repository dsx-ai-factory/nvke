// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// templateDoc parses the embedded template and returns its document node.
//
// The document rather than the mapping, because yaml.v3 hangs the file's
// leading comments — the licence header and the explanation of what the file is
// — off the document. Encoding the mapping alone silently drops them.
//
// The template is parsed rather than templated so every comment survives, which
// is what lets a generated file explain itself the way the checked-in example
// does.
func templateDoc() (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(templateYAML, &doc); err != nil {
		return nil, fmt.Errorf("parse site spec template: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("parse site spec template: expected one document")
	}
	return &doc, nil
}

// checkSettable reports whether --set may write path.
func checkSettable(path string) error {
	if reason, denied := deniedPaths()[path]; denied {
		return fmt.Errorf("--set %s: %s", path, reason)
	}
	for _, settable := range SettablePaths() {
		if path == settable {
			return nil
		}
	}
	return fmt.Errorf("--set %s: unknown field; settable fields are %s",
		path, strings.Join(SettablePaths(), ", "))
}

// setScalar writes value at a dotted path, creating the key and any missing
// parent when the template only carries it as a commented-out example.
func setScalar(root *yaml.Node, path, value string) error {
	keys := strings.Split(path, ".")
	node := root
	for _, key := range keys[:len(keys)-1] {
		child, err := childMapping(node, key)
		if err != nil {
			return fmt.Errorf("set %s: %w", path, err)
		}
		node = child
	}

	leaf := keys[len(keys)-1]
	dropCommentedKey(node, leaf)
	if valueNode := mappingValue(node, leaf); valueNode != nil {
		valueNode.SetString(value)
		return nil
	}
	appendMapping(node, leaf, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle})
	return nil
}

// setNodes replaces cluster.nodes with the CSV rows, in file order.
func setNodes(root *yaml.Node, nodes []Node) error {
	cluster, err := childMapping(root, "cluster")
	if err != nil {
		return fmt.Errorf("set %s: %w", pathNodes, err)
	}

	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, node := range nodes {
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendMapping(entry, "name", scalar(node.Name))
		appendMapping(entry, "ip", scalar(node.IP))
		if node.TPMEKThumbprint != "" {
			appendMapping(entry, "tpmEkThumbprint", scalar(node.TPMEKThumbprint))
		}
		seq.Content = append(seq.Content, entry)
	}

	if existing := mappingValue(cluster, "nodes"); existing != nil {
		existing.Kind, existing.Tag, existing.Style = seq.Kind, seq.Tag, 0
		existing.Content, existing.Value = seq.Content, ""
		return nil
	}
	appendMapping(cluster, "nodes", seq)
	return nil
}

// applySpec writes every field of spec onto the template tree, so a rendered
// file reflects the validated value rather than the template's example.
func applySpec(root *yaml.Node, spec *SiteSpec) error {
	if err := setNodes(root, spec.Cluster.Nodes); err != nil {
		return err
	}
	for path, value := range map[string]string{
		pathClusterName:          spec.ClusterName,
		pathK8sVersion:           spec.Cluster.K8sVersion,
		pathCniName:              spec.Cluster.CniName,
		pathPodCidr:              spec.Cluster.PodCidr,
		pathServiceCidr:          spec.Cluster.ServiceCidr,
		pathSystemReservedMemory: spec.Cluster.KubeletConfig.SystemReservedMemory,
		pathBootstrapRepo:        spec.Registry.BootstrapRepo,
		pathBootstrapChart:       spec.Registry.BootstrapChart,
	} {
		if err := setScalar(root, path, value); err != nil {
			return err
		}
	}
	// Optional fields are written only when set, so an unset one stays a
	// commented example rather than becoming an empty string the loader would
	// then have to treat as absent.
	nodeAgentPort := spec.Cluster.NodeAgentPort
	if nodeAgentPort == DefaultNodeAgentPort {
		// SetDefaults fills this in, but the template carries it as a commented
		// example. Writing back a value the loader would supply anyway would
		// make every generated file pin a default it never chose.
		nodeAgentPort = ""
	}
	for path, value := range map[string]string{
		pathCniVersion:      spec.Cluster.CniVersion,
		pathNodeAgentPort:   nodeAgentPort,
		pathSpireCACertFile: spec.Spire.CACertFile,
		pathSpireCAKeyFile:  spec.Spire.CAKeyFile,
	} {
		if value == "" {
			continue
		}
		if err := setScalar(root, path, value); err != nil {
			return err
		}
	}
	return nil
}

// decode reads a SiteSpec out of a node tree.
func decode(root *yaml.Node) (*SiteSpec, error) {
	var spec SiteSpec
	if err := root.Decode(&spec); err != nil {
		return nil, fmt.Errorf("decode site spec: %w", err)
	}
	return &spec, nil
}

// decodeBytes reads a SiteSpec out of rendered YAML, rejecting any field the
// struct does not declare, as Load does.
func decodeBytes(b []byte) (*SiteSpec, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	var spec SiteSpec
	if err := decoder.Decode(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// specDiff returns a human-readable difference between two site specs, or "".
func specDiff(got, want *SiteSpec) string {
	if reflect.DeepEqual(got, want) {
		return ""
	}
	gotYAML, _ := yaml.Marshal(got)
	wantYAML, _ := yaml.Marshal(want)
	return fmt.Sprintf("\n--- rendered\n%s\n--- validated\n%s", gotYAML, wantYAML)
}

func scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
}

// mappingValue returns the value node for key, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// childMapping returns the mapping at key, creating it when the template
// carries the block only as a comment.
func childMapping(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if value := mappingValue(mapping, key); value != nil {
		if value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%q is not a mapping", key)
		}
		return value, nil
	}
	dropCommentedKey(mapping, key)
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendMapping(mapping, key, child)
	return child, nil
}

func appendMapping(mapping *yaml.Node, key string, value *yaml.Node) {
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value)
}

// dropCommentedKey removes a commented-out example of key from the comments
// around a mapping, so setting the field does not leave the file carrying both
// the real value and a stale "# key:" line above it.
func dropCommentedKey(mapping *yaml.Node, key string) {
	pattern := regexp.MustCompile(`^\s*#\s*` + regexp.QuoteMeta(key) + `\s*:`)
	strip := func(comment string) string {
		if comment == "" {
			return ""
		}
		lines := strings.Split(comment, "\n")
		kept := lines[:0]
		dropping := false
		for _, line := range lines {
			switch {
			case pattern.MatchString(line):
				// Drop this line and any deeper-indented continuation, which is
				// how a commented-out block writes its own children.
				dropping = true
			case dropping && regexp.MustCompile(`^\s*#\s+\S`).MatchString(line):
				continue
			default:
				dropping = false
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}

	for _, node := range append([]*yaml.Node{mapping}, mapping.Content...) {
		node.HeadComment = strip(node.HeadComment)
		node.FootComment = strip(node.FootComment)
		node.LineComment = strip(node.LineComment)
	}
}
