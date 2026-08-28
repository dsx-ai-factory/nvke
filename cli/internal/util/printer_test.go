// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseFormat(t *testing.T) {
	tests := []struct {
		input   string
		want    Format
		wantErr bool
	}{
		{"", FormatTable, false},
		{"json", FormatJSON, false},
		{"yaml", FormatYAML, false},
		{"xml", FormatTable, true},
		{"JSON", FormatTable, true},
		{"table", FormatTable, true},
	}
	for _, tt := range tests {
		got, err := ParseFormat(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseFormat(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseFormat(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestPrint_JSON(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	var buf bytes.Buffer
	if err := Print(&buf, FormatJSON, payload{Name: "my-cluster"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"name": "my-cluster"`) {
		t.Errorf("unexpected JSON output: %s", buf.String())
	}
}

func TestPrint_YAML(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	var buf bytes.Buffer
	if err := Print(&buf, FormatYAML, payload{Name: "my-cluster"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "name: my-cluster") {
		t.Errorf("unexpected YAML output: %s", buf.String())
	}
}

func TestPrint_TableReturnsError(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(&buf, FormatTable, struct{}{}); err == nil {
		t.Error("expected error for table format, got nil")
	}
}

func TestPrintFields_FlatAndNested(t *testing.T) {
	type inner struct {
		Count int `json:"count"`
	}
	type payload struct {
		Name   string `json:"name"`
		Detail inner  `json:"detail"`
	}
	var buf bytes.Buffer
	if err := PrintFields(&buf, payload{Name: "x", Detail: inner{Count: 3}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "name: x") {
		t.Errorf("missing name in output: %s", out)
	}
	if !strings.Contains(out, "detail:") {
		t.Errorf("missing nested key in output: %s", out)
	}
	if !strings.Contains(out, "count: 3") {
		t.Errorf("missing nested value in output: %s", out)
	}
}

func TestPrintFields_NilValue(t *testing.T) {
	type payload struct {
		Val *string `json:"val"`
	}
	var buf bytes.Buffer
	if err := PrintFields(&buf, payload{Val: nil}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "val: null") {
		t.Errorf("nil field not rendered as null: %s", buf.String())
	}
}

func TestPrintFields_ArrayItems(t *testing.T) {
	type payload struct {
		Tags []string `json:"tags"`
	}
	var buf bytes.Buffer
	if err := PrintFields(&buf, payload{Tags: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "- a") || !strings.Contains(out, "- b") {
		t.Errorf("array items not rendered: %s", out)
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		age  time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m"},
		{90 * time.Minute, "1h"},
		{48 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		got := FormatAge(now.Add(-tt.age))
		if got != tt.want {
			t.Errorf("FormatAge(-%v) = %q, want %q", tt.age, got, tt.want)
		}
	}
}

func TestFormatAge_Future(t *testing.T) {
	got := FormatAge(time.Now().Add(10 * time.Minute))
	if got != "0s" {
		t.Errorf("future time should report 0s, got %q", got)
	}
}
