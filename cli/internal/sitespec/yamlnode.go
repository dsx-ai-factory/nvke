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
// parent when the template carries it only as a commented-out example.
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
	if valueNode := mappingValue(node, leaf); valueNode != nil {
		valueNode.SetString(value)
		return nil
	}
	putMapping(node, leaf, scalar(value))
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
		putMapping(entry, "name", scalar(node.Name))
		putMapping(entry, "ip", scalar(node.IP))
		if node.TPMEKThumbprint != "" {
			putMapping(entry, "tpmEkThumbprint", scalar(node.TPMEKThumbprint))
		}
		seq.Content = append(seq.Content, entry)
	}

	if existing := mappingValue(cluster, "nodes"); existing != nil {
		existing.Kind, existing.Tag, existing.Style = seq.Kind, seq.Tag, 0
		existing.Content, existing.Value = seq.Content, ""
		return nil
	}
	putMapping(cluster, "nodes", seq)
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
		pathCniVersion:       spec.Cluster.CniVersion,
		pathNodeAgentPort:    nodeAgentPort,
		pathSpireTrustDomain: spec.Spire.TrustDomain,
		pathSpireCACertFile:  spec.Spire.CACertFile,
		pathSpireCAKeyFile:   spec.Spire.CAKeyFile,
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
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	putMapping(mapping, key, child)
	return child, nil
}

// putMapping adds key to mapping.
//
// Where the template carries key as a commented-out example, the real entry
// takes that example's place and inherits the prose introducing it. Appending
// instead would leave the explanation stranded above whatever followed it and
// put the field at the end of the file, far from the section it belongs to.
func putMapping(mapping *yaml.Node, key string, value *yaml.Node) {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}

	at, prose, found := takeCommentedExample(mapping, key)
	if !found {
		mapping.Content = append(mapping.Content, keyNode, value)
		return
	}

	keyNode.HeadComment = prose
	rest := append([]*yaml.Node{}, mapping.Content[at:]...)
	mapping.Content = append(append(mapping.Content[:at:at], keyNode, value), rest...)
}

// commentedKeyLine matches a commented-out example of key, e.g. "# spire:".
func commentedKeyLine(key string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*#\s*` + regexp.QuoteMeta(key) + `\s*:`)
}

// commentedChildLine matches a line belonging to the example above it: a
// comment indented further than the key that introduced it.
var commentedChildLine = regexp.MustCompile(`^\s*#\s{2,}\S`)

// takeCommentedExample removes the commented-out example of key from the
// comments of mapping's entries, and reports where the real key belongs along
// with the prose that introduced the example.
//
// An example may sit above an entry (its head comment) or below one (its foot
// comment); the real key goes in the same place, so it lands where the template
// showed it rather than at the end of the section. The prose moves to the new
// key; anything after the example stays where it was, which is how the
// following section keeps its own explanation.
func takeCommentedExample(mapping *yaml.Node, key string) (at int, prose string, found bool) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		node := mapping.Content[i]

		if kept, taken, ok := splitCommentedExample(node.HeadComment, key); ok {
			node.HeadComment = kept
			return i, taken, true
		}
		if kept, taken, ok := splitCommentedExample(node.FootComment, key); ok {
			node.FootComment = kept
			return i + 2, taken, true
		}
	}
	return 0, "", false
}

// splitCommentedExample removes key's commented-out example from comment,
// returning what is left and the prose that introduced it.
func splitCommentedExample(comment, key string) (kept, prose string, found bool) {
	if comment == "" {
		return comment, "", false
	}
	lines := strings.Split(comment, "\n")
	keyLine := commentedKeyLine(key)

	example := -1
	for j, line := range lines {
		if keyLine.MatchString(line) {
			example = j
			break
		}
	}
	if example < 0 {
		return comment, "", false
	}

	// The example's own block starts after the last blank line before it, so a
	// preceding section's comment is not taken with it.
	start := 0
	for j := example - 1; j >= 0; j-- {
		if strings.TrimSpace(lines[j]) == "" {
			start = j + 1
			break
		}
	}

	end := example
	for j := example + 1; j < len(lines) && commentedChildLine.MatchString(lines[j]); j++ {
		end = j
	}
	drop := end + 1
	if drop < len(lines) && strings.TrimSpace(lines[drop]) == "" {
		drop++
	}

	remaining := append(append([]string{}, lines[:start]...), lines[drop:]...)
	return strings.Join(remaining, "\n"), strings.Join(lines[start:example], "\n"), true
}
