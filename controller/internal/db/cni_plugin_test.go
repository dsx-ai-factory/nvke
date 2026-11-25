// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestDefaultCNIVersion(t *testing.T) {
	tests := []struct {
		name        string
		plugin      db.CniPlugin
		wantVersion string
		wantOK      bool
	}{
		{name: "calico has a default", plugin: db.CNICalico, wantVersion: db.DefaultCalicoCNIVersion, wantOK: true},
		{name: "ovn-kubernetes has a default", plugin: db.CNIOvnKubernetes, wantVersion: db.DefaultOvnKubernetesCNIVersion, wantOK: true},
		{name: "an unknown plugin has no default", plugin: db.CniPlugin("cilium")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, ok := db.DefaultCNIVersion(tt.plugin)
			assert.Equal(t, tt.wantVersion, version)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

func TestEffectiveCNIVersion(t *testing.T) {
	tests := []struct {
		name    string
		plugin  db.CniPlugin
		version string
		want    string
	}{
		{name: "ovn-kubernetes falls back to its default", plugin: db.CNIOvnKubernetes, want: db.DefaultOvnKubernetesCNIVersion},
		{name: "calico falls back to its default", plugin: db.CNICalico, want: db.DefaultCalicoCNIVersion},
		{name: "a blank version falls back to the default", plugin: db.CNIOvnKubernetes, version: "  ", want: db.DefaultOvnKubernetesCNIVersion},
		{name: "an explicit version wins and is trimmed", plugin: db.CNIOvnKubernetes, version: " 1.4.0 ", want: "1.4.0"},
		{name: "an unknown plugin has no default version", plugin: db.CniPlugin("cilium")},
		{name: "an unknown plugin keeps a version it was given", plugin: db.CniPlugin("cilium"), version: "1.0.0", want: "1.0.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, db.EffectiveCNIVersion(tt.plugin, tt.version))
		})
	}
}

func TestEffectiveCNIPlugin(t *testing.T) {
	tests := []struct {
		name   string
		plugin db.CniPlugin
		want   db.CniPlugin
	}{
		{name: "empty defaults to ovn-kubernetes", plugin: "", want: db.CNIOvnKubernetes},
		{name: "blank defaults to ovn-kubernetes", plugin: "  ", want: db.CNIOvnKubernetes},
		{name: "ovn-kubernetes is kept", plugin: db.CNIOvnKubernetes, want: db.CNIOvnKubernetes},
		{name: "calico is kept", plugin: db.CNICalico, want: db.CNICalico},
		{name: "a plugin that is not listed is kept, because validation is the caller's job", plugin: db.CniPlugin("cilium"), want: db.CniPlugin("cilium")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, db.EffectiveCNIPlugin(tt.plugin))
		})
	}
}

func TestListCNIPlugins(t *testing.T) {
	assert.ElementsMatch(t, []db.CniPlugin{db.CNICalico, db.CNIOvnKubernetes}, db.ListCNIPlugins())
}
