// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeadmv1beta3 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
)

func TestUsesKubeadmV1beta4(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		want              bool
	}{
		{name: "empty keeps v1beta3", kubernetesVersion: "", want: false},
		{name: "unparseable keeps v1beta3", kubernetesVersion: "latest", want: false},
		{name: "1.30 keeps v1beta3", kubernetesVersion: "v1.30.14", want: false},
		{name: "1.31 is the first v1beta4 release", kubernetesVersion: "1.31.0", want: true},
		{name: "1.34 renders v1beta4", kubernetesVersion: "v1.34.2", want: true},
		{name: "1.37 renders v1beta4", kubernetesVersion: "1.37.0", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, usesKubeadmV1beta4(tt.kubernetesVersion))
		})
	}
}

func TestInitConfigRenderKubeadmAPIVersion(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		contains          []string
		notContains       []string
	}{
		{
			name:              "1.30 renders v1beta3 with the control-plane wait on apiServer",
			kubernetesVersion: "v1.30.14",
			contains: []string{
				"apiVersion: kubeadm.k8s.io/v1beta3",
				"timeoutForControlPlane: 4m0s",
				"node-ip: 192.168.1.10",
			},
			notContains: []string{"kubeadm.k8s.io/v1beta4", "timeouts:"},
		},
		{
			name:              "1.37 renders v1beta4 with the control-plane wait under timeouts",
			kubernetesVersion: "v1.37.0",
			contains: []string{
				"apiVersion: kubeadm.k8s.io/v1beta4",
				"timeouts:\n  controlPlaneComponentHealthCheck: 4m0s",
				"- name: node-ip\n    value: 192.168.1.10",
			},
			notContains: []string{"kubeadm.k8s.io/v1beta3", "timeoutForControlPlane"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validInitConfig()
			cfg.KubernetesVersion = tt.kubernetesVersion

			artifacts, err := cfg.Render()
			require.NoError(t, err)
			rendered := string(artifacts.ConfigYAML)
			for _, want := range tt.contains {
				assert.Contains(t, rendered, want)
			}
			for _, unwanted := range tt.notContains {
				assert.NotContains(t, rendered, unwanted)
			}
		})
	}
}

func TestJoinConfigRenderKubeadmAPIVersion(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		contains          []string
		notContains       []string
	}{
		{
			name:              "no version keeps v1beta3 with discovery.timeout",
			kubernetesVersion: "",
			contains:          []string{"apiVersion: kubeadm.k8s.io/v1beta3", "  timeout: 5m0s"},
			notContains:       []string{"kubeadm.k8s.io/v1beta4", "timeouts:"},
		},
		{
			name:              "1.37 renders v1beta4 with the discovery wait under timeouts",
			kubernetesVersion: "v1.37.0",
			contains:          []string{"apiVersion: kubeadm.k8s.io/v1beta4", "timeouts:\n  discovery: 5m0s"},
			notContains:       []string{"kubeadm.k8s.io/v1beta3", "  timeout: 5m0s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validWorkerJoinConfig()
			cfg.KubernetesVersion = tt.kubernetesVersion

			artifacts, err := cfg.Render()
			require.NoError(t, err)
			rendered := string(artifacts.ConfigYAML)
			for _, want := range tt.contains {
				assert.Contains(t, rendered, want)
			}
			for _, unwanted := range tt.notContains {
				assert.NotContains(t, rendered, unwanted)
			}
		})
	}
}

// TestV1beta4ConversionKeepsEveryField fills every field of the v1beta3 documents
// with a unique value and checks that each value reaches the v1beta4 output. The
// converter copies field by field, so a field added to the v1beta3 structs would
// otherwise compile and then silently disappear from every v1beta4 config.
func TestV1beta4ConversionKeepsEveryField(t *testing.T) {
	t.Run("init and cluster configuration", func(t *testing.T) {
		filler := &uniqueFiller{t: t}
		var initConfig kubeadmv1beta3.InitConfiguration
		var clusterConfig kubeadmv1beta3.ClusterConfiguration
		filler.fill(reflect.ValueOf(&initConfig).Elem(), "InitConfiguration", "")
		filler.fill(reflect.ValueOf(&clusterConfig).Elem(), "ClusterConfiguration", "")

		assertCarriesEveryValue(t, filler.wants,
			initConfigurationV1beta4(initConfig, clusterConfig),
			clusterConfigurationV1beta4(clusterConfig),
		)
	})
	t.Run("join configuration", func(t *testing.T) {
		filler := &uniqueFiller{t: t}
		var joinConfig kubeadmv1beta3.JoinConfiguration
		filler.fill(reflect.ValueOf(&joinConfig).Elem(), "JoinConfiguration", "")

		assertCarriesEveryValue(t, filler.wants, joinConfigurationV1beta4(joinConfig))
	})
}

// wantedValue is the text a filled v1beta3 field must produce in the converted
// JSON, and the field it came from.
type wantedValue struct {
	text  string
	field string
}

// uniqueFiller sets every exported field to a value that appears nowhere else,
// and records the text each value must produce in the converted JSON.
type uniqueFiller struct {
	t     *testing.T
	n     int
	wants []wantedValue
}

func (f *uniqueFiller) next() int {
	f.n++
	return f.n
}

func (f *uniqueFiller) want(text, field string) {
	f.wants = append(f.wants, wantedValue{text: text, field: field})
}

func (f *uniqueFiller) fill(v reflect.Value, field, jsonName string) {
	f.t.Helper()
	switch v.Type() {
	case reflect.TypeFor[metav1.TypeMeta]():
		return // the converter sets apiVersion itself
	case reflect.TypeFor[metav1.Time]():
		return // only reached through types copied whole, such as BootstrapToken
	case reflect.TypeFor[metav1.Duration]():
		d := time.Duration(f.next()) * time.Minute
		v.Set(reflect.ValueOf(metav1.Duration{Duration: d}))
		f.want(fmt.Sprintf("%q", d.String()), field)
		return
	}

	switch v.Kind() { //nolint:exhaustive // the default fails the test for any kind the v1beta3 structs do not use
	case reflect.String:
		s := fmt.Sprintf("v3f%03d", f.next())
		v.SetString(s)
		f.want(s, field)
	case reflect.Bool:
		v.SetBool(true)
		f.want(fmt.Sprintf("%q:true", jsonName), field)
	case reflect.Int, reflect.Int32, reflect.Int64:
		n := 50000 + f.next()
		v.SetInt(int64(n))
		f.want(fmt.Sprintf(":%d", n), field)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		f.fill(p.Elem(), field, jsonName)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		f.fill(s.Index(0), field+"[0]", jsonName)
		v.Set(s)
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		f.fill(key, field+" key", jsonName)
		value := reflect.New(v.Type().Elem()).Elem()
		f.fill(value, field+" value", key.String())
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, value)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			sf := v.Type().Field(i)
			if !sf.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			f.fill(v.Field(i), field+"."+sf.Name, name)
		}
	default:
		f.t.Fatalf("uniqueFiller cannot fill %s of kind %s; extend it", field, v.Kind())
	}
}

func assertCarriesEveryValue(t *testing.T, wants []wantedValue, documents ...any) {
	t.Helper()
	var converted strings.Builder
	for _, document := range documents {
		encoded, err := json.Marshal(document)
		require.NoError(t, err)
		converted.Write(encoded)
	}
	require.NotEmpty(t, wants)
	for _, want := range wants {
		if !strings.Contains(converted.String(), want.text) {
			t.Errorf("v1beta3 %s is missing from the v1beta4 output; copy it in v1beta4.go", want.field)
		}
	}
}
