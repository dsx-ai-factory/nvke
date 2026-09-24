// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestUnit_Quote(t *testing.T) {
	t.Parallel()

	value := "root:root # YAML-sensitive\nnext line"
	quoted := Quote(value)

	var decoded string
	require.NoError(t, yaml.Unmarshal([]byte(quoted), &decoded))
	assert.Equal(t, value, decoded)
}

func TestIndent_Empty(t *testing.T) {
	result := Indent(5, "")
	assert.Empty(t, result)
}

func TestIndent_SingleLine(t *testing.T) {
	result := Indent(5, "foo")
	assert.Equal(t, "     foo", result)
}

func TestIndent_MultipleLines(t *testing.T) {
	result := Indent(5, "foo\nbar\nbaz\n")
	expected := `     foo
     bar
     baz
`
	assert.Equal(t, expected, result)
}

func TestIndent_NoTrailingNewline(t *testing.T) {
	result := Indent(5, "foo\nbar")
	expected := `     foo
     bar`
	assert.Equal(t, expected, result)
}

func TestIndent_TrailingNewline(t *testing.T) {
	result := Indent(5, "foo\nbar\n")
	expected := `     foo
     bar
`
	assert.Equal(t, expected, result)
}

func TestIndent_MultipleTrailingNewlines(t *testing.T) {
	result := Indent(5, "foo\nbar\n\n\n")
	expected := `     foo
     bar


`
	assert.Equal(t, expected, result)
}

func TestIndent_YAMLBlockScalarTrailingNewlines(t *testing.T) {
	input := `foo
bar


`
	indented := Indent(10, input)
	doc := `data:
  content: |
` + indented

	var parsed map[string]interface{}
	err := yaml.Unmarshal([]byte(doc), &parsed)
	require.NoError(t, err)

	result := parsed["data"].(map[string]interface{})["content"].(string)

	// YAML literal block scalar (|) uses "clip" chomping: strips trailing empty lines
	expected := `foo
bar
`
	assert.Equal(t, expected, result)
}

func TestIndent_YAMLBlockScalarWithMultipleEmptyLines(t *testing.T) {
	input := `line1
line2


line3`
	indented := Indent(8, input)
	doc := `data:
  content: |
` + indented

	var parsed map[string]interface{}
	err := yaml.Unmarshal([]byte(doc), &parsed)
	require.NoError(t, err)

	result := parsed["data"].(map[string]interface{})["content"].(string)

	assert.Equal(t, input, result)
}

func TestIndent_YAMLBlockScalarEmptyLinesDoNotTerminateBlock(t *testing.T) {
	input := `first

second`
	indented := Indent(10, input)
	doc := `files:
  - name: test
    content: |
` + indented + `
  - name: another
    content: data`

	var parsed map[string]interface{}
	err := yaml.Unmarshal([]byte(doc), &parsed)
	require.NoError(t, err)

	files := parsed["files"].([]interface{})
	assert.Len(t, files, 2)

	firstFile := files[0].(map[string]interface{})
	secondFile := files[1].(map[string]interface{})

	// YAML adds trailing newline due to clip chomping
	expected := input + "\n"
	assert.Equal(t, expected, firstFile["content"].(string))
	assert.Equal(t, "test", firstFile["name"])
	assert.Equal(t, "another", secondFile["name"])
	assert.Equal(t, "data", secondFile["content"])
}
