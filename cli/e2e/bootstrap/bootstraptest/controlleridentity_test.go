// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

const testControllerSPIFFEID = "spiffe://nvkectl-e2e.example.test/nvke-service/nvke-controller"

// testCA returns a self-signed CA certificate, PEM encoded, named cn.
func testCA(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// controllerObjects returns the controller's Deployment running as
// serviceAccount and its external Service's EndpointSlice with one endpoint
// that is ready or not.
func controllerObjects(serviceAccount string, ready bool) []runtime.Object {
	return []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-controller", Name: "nvke-controller"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{ServiceAccountName: serviceAccount},
			}},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "nvke-controller",
				Name:      "nvke-controller-external-x7k2p",
				Labels:    map[string]string{discoveryv1.LabelServiceName: "nvke-controller-external"},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.244.0.12"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			}},
		},
	}
}

// fakeSpire answers `entry show -spiffeID testControllerSPIFFEID` with one
// entry when hasEntry, any other entry query with none, and `bundle show` with
// bundle.
func fakeSpire(hasEntry bool, bundle []byte) SpireServerCLI {
	return func(_ context.Context, args ...string) (string, error) {
		switch {
		case hasEntry && slices.Equal(args, []string{"entry", "show", "-spiffeID", testControllerSPIFFEID}):
			return "Found 1 entry\n" +
				"Entry ID         : nvkectl-bootstrap.ad296e11-ad77-43f4-9e17-5b5c81270dad\n" +
				"SPIFFE ID        : " + testControllerSPIFFEID + "\n" +
				"Parent ID        : spiffe://nvkectl-e2e.example.test/spire/agent/k8s_psat/nvkectl-bootstrap/4cec5dae\n", nil
		case len(args) > 0 && args[0] == "entry":
			return "Found 0 entries\n", nil
		case slices.Equal(args, []string{"bundle", "show"}):
			return string(bundle), nil
		}
		return "", errors.New("unexpected spire-server arguments")
	}
}

func TestVerifyControllerIdentity(t *testing.T) {
	upstream, other := testCA(t, "upstream"), testCA(t, "spire-self-signed")
	want := ControllerIdentity{Name: "nvke-controller", SPIFFEID: testControllerSPIFFEID}
	withCA := want
	withCA.UpstreamCA = upstream

	tests := []struct {
		name    string
		objects []runtime.Object
		spire   SpireServerCLI
		want    ControllerIdentity
		wantErr []string
	}{
		{
			name:    "the controller runs as its ServiceAccount and SPIRE holds its entry",
			objects: controllerObjects("nvke-controller", true), spire: fakeSpire(true, other), want: want,
		},
		{
			name:    "SPIRE's bundle holds the upstream CA",
			objects: controllerObjects("nvke-controller", true), spire: fakeSpire(true, append(other, upstream...)), want: withCA,
		},
		{
			name:    "the controller runs as another ServiceAccount",
			objects: controllerObjects("default", true), spire: fakeSpire(true, nil), want: want,
			wantErr: []string{"nvke-controller/nvke-controller runs as ServiceAccount default, not nvke-controller"},
		},
		{
			name:    "the external Service has no ready endpoint",
			objects: controllerObjects("nvke-controller", false), spire: fakeSpire(true, nil), want: want,
			wantErr: []string{"Service nvke-controller/nvke-controller-external has no ready endpoint"},
		},
		{
			name:    "SPIRE has no entry for the controller",
			objects: controllerObjects("nvke-controller", true), spire: fakeSpire(false, nil), want: want,
			wantErr: []string{"SPIRE has no entry for " + testControllerSPIFFEID},
		},
		{
			name:    "SPIRE's bundle does not hold the upstream CA",
			objects: controllerObjects("nvke-controller", true), spire: fakeSpire(true, other), want: withCA,
			wantErr: []string{"SPIRE's bundle does not hold the upstream CA"},
		},
		{
			name:    "every problem is named",
			objects: controllerObjects("default", false), spire: fakeSpire(false, other), want: withCA,
			wantErr: []string{"runs as ServiceAccount default", "has no ready endpoint", "SPIRE has no entry", "does not hold the upstream CA"},
		},
		{
			name:    "the controller Deployment is missing",
			objects: nil, spire: fakeSpire(true, nil), want: want,
			wantErr: []string{"reading deployment nvke-controller/nvke-controller"},
		},
		{
			name:    "the SPIRE server cannot be asked",
			objects: controllerObjects("nvke-controller", true), want: want,
			spire: func(context.Context, ...string) (string, error) {
				return "", errors.New(`pods "spire-server-0" not found`)
			},
			wantErr: []string{"asking SPIRE for the entry of " + testControllerSPIFFEID, `pods "spire-server-0" not found`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyControllerIdentity(context.Background(), fake.NewClientset(tt.objects...), tt.spire, tt.want)
			if len(tt.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestControllerIdentityFromEnv(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	ca := testCA(t, "upstream")
	require.NoError(t, os.WriteFile(caFile, ca, 0o600))

	tests := []struct {
		name    string
		env     map[string]string
		want    ControllerIdentity
		wantErr string
	}{
		{name: "unset asks for no check"},
		{
			name: "the lane names the controller and its SPIFFE ID",
			env:  map[string]string{EnvControllerSPIFFEID: testControllerSPIFFEID, EnvBootstrapCluster: "nvkectl-bootstrap", EnvControllerName: "nvke-controller"},
			want: ControllerIdentity{Name: "nvke-controller", SPIFFEID: testControllerSPIFFEID},
		},
		{
			name: "a supplied upstream CA is expected in SPIRE's bundle",
			env: map[string]string{
				EnvControllerSPIFFEID: testControllerSPIFFEID, EnvBootstrapCluster: "nvkectl-bootstrap", EnvControllerName: "nvke-controller",
				sitespec.EnvSpireCACertFile: caFile,
			},
			want: ControllerIdentity{Name: "nvke-controller", SPIFFEID: testControllerSPIFFEID, UpstreamCA: ca},
		},
		{
			name:    "without the bootstrap cluster",
			env:     map[string]string{EnvControllerSPIFFEID: testControllerSPIFFEID, EnvControllerName: "nvke-controller"},
			wantErr: EnvBootstrapCluster + " is not",
		},
		{
			name:    "without the controller name",
			env:     map[string]string{EnvControllerSPIFFEID: testControllerSPIFFEID, EnvBootstrapCluster: "nvkectl-bootstrap"},
			wantErr: EnvControllerName + " is not",
		},
		{
			name: "an unreadable upstream CA",
			env: map[string]string{
				EnvControllerSPIFFEID: testControllerSPIFFEID, EnvBootstrapCluster: "nvkectl-bootstrap", EnvControllerName: "nvke-controller",
				sitespec.EnvSpireCACertFile: filepath.Join(t.TempDir(), "missing.crt"),
			},
			wantErr: "reading the SPIRE upstream CA",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{EnvControllerSPIFFEID, EnvBootstrapCluster, EnvControllerName, sitespec.EnvSpireCACertFile} {
				t.Setenv(name, tt.env[name])
			}
			got, err := ControllerIdentityFromEnv()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
