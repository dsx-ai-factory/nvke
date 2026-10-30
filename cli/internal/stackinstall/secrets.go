// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

// Names no values file carries: the Temporal chart has no namespaceOverride,
// CAPNICo reads its Secret from a manifest, and the overlay names SPIRE's CA
// Secret.
const (
	temporalNamespace = "temporal"
	capnicoNamespace  = "capnico-system"
	nicoSecret        = "nico-credentials"
	spireCASecret     = "spire-upstream-ca"
)

// NICoAPI is the NICo API that CAPNICo calls.
type NICoAPI struct {
	Endpoint string
	OrgID    string
	APIName  string
}

// secretRefs are the Secret names, keys and namespaces the values files
// reference. The charts read the same files, so Go restates none of them.
type secretRefs struct {
	controller struct {
		NamespaceOverride string `yaml:"namespaceOverride"`
		Secrets           struct {
			DBSecretName             string `yaml:"dbSecretName"`
			DBSecretKey              string `yaml:"dbSecretKey"`
			DGXCTenantCredSecretName string `yaml:"dgxcTenantCredSecretName"`
		} `yaml:"secrets"`
		AppDB struct {
			URL          string `yaml:"url"`
			DBName       string `yaml:"dbName"`
			PostgresUser string `yaml:"postgresUser"`
			SSLMode      string `yaml:"sslMode"`
		} `yaml:"appDb"`
	}
	postgresql struct {
		NamespaceOverride string `yaml:"namespaceOverride"`
		Auth              struct {
			ExistingSecret string `yaml:"existingSecret"`
			SecretKeys     struct {
				AdminPasswordKey string `yaml:"adminPasswordKey"`
			} `yaml:"secretKeys"`
		} `yaml:"auth"`
	}
	temporal struct {
		Server struct {
			Config struct {
				Persistence struct {
					Default struct {
						SQL struct {
							ExistingSecret string `yaml:"existingSecret"`
							SecretKey      string `yaml:"secretKey"`
						} `yaml:"sql"`
					} `yaml:"default"`
				} `yaml:"persistence"`
			} `yaml:"config"`
		} `yaml:"server"`
	}
	spire struct {
		Server struct {
			NamespaceOverride string `yaml:"namespaceOverride"`
		} `yaml:"spire-server"`
	}
}

func readSecretRefs() (*secretRefs, error) {
	var r secretRefs
	for file, into := range map[string]any{
		"values/controller.yaml": &r.controller,
		"values/postgresql.yaml": &r.postgresql,
		"values/temporal.yaml":   &r.temporal,
		"values/spire.yaml":      &r.spire,
	} {
		if err := readValues(file, into); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// CredentialSecrets returns the Secrets the stack's charts read. Each carries
// its values in data, not stringData, so a server-side apply can update it.
func CredentialSecrets(c Credentials, nico NICoAPI) ([]*corev1.Secret, error) {
	for _, f := range []struct{ name, value string }{
		{"NICoAPI.Endpoint", nico.Endpoint},
		{"NICoAPI.OrgID", nico.OrgID},
		{"NICoAPI.APIName", nico.APIName},
		{"Credentials.DatabasePassword", c.DatabasePassword},
		{"Credentials.NicoToken", c.NicoToken},
		{"Credentials.NodeUser", c.NodeUser},
		{"Credentials.NodePassword", c.NodePassword},
	} {
		if f.value == "" {
			return nil, fmt.Errorf("stackinstall: %s is required", f.name)
		}
	}
	if (len(c.SpireCACert) > 0) != (len(c.SpireCAKey) > 0) {
		return nil, errors.New("stackinstall: Credentials.SpireCACert and SpireCAKey must be set together")
	}
	r, err := readSecretRefs()
	if err != nil {
		return nil, err
	}
	// Base64 of the URL does not contain base64 of the password, so the
	// connection string is registered whole in both forms.
	conn := r.connectionString(c.DatabasePassword)
	redact.RegisterSecret(conn)
	redact.RegisterSecret(base64.StdEncoding.EncodeToString([]byte(conn)))

	password := []byte(c.DatabasePassword)
	temporalSQL := r.temporal.Server.Config.Persistence.Default.SQL
	secrets := []*corev1.Secret{
		secret(r.postgresql.NamespaceOverride, r.postgresql.Auth.ExistingSecret, map[string][]byte{r.postgresql.Auth.SecretKeys.AdminPasswordKey: password}),
		secret(temporalNamespace, temporalSQL.ExistingSecret, map[string][]byte{temporalSQL.SecretKey: password}),
		secret(r.controller.NamespaceOverride, r.controller.Secrets.DBSecretName, map[string][]byte{r.controller.Secrets.DBSecretKey: []byte(conn)}),
		secret(r.controller.NamespaceOverride, r.controller.Secrets.DGXCTenantCredSecretName, map[string][]byte{"user": []byte(c.NodeUser), "password": []byte(c.NodePassword)}),
		secret(capnicoNamespace, nicoSecret, map[string][]byte{
			"endpoint": []byte(nico.Endpoint), "orgID": []byte(nico.OrgID), "apiName": []byte(nico.APIName), "token": []byte(c.NicoToken),
		}),
	}
	if len(c.SpireCACert) > 0 {
		// A self-signed root is its own bundle.
		secrets = append(secrets, secret(r.spire.Server.NamespaceOverride, spireCASecret, map[string][]byte{
			"tls.crt": c.SpireCACert, "tls.key": c.SpireCAKey, "bundle.crt": c.SpireCACert,
		}))
	}
	return secrets, nil
}

func secret(namespace, name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// connectionString is the controller's database URL, as appDb describes it.
func (r *secretRefs) connectionString(password string) string {
	db := r.controller.AppDB
	return fmt.Sprintf("postgresql://%s@%s/%s?sslmode=%s", url.UserPassword(db.PostgresUser, password).String(), db.URL, db.DBName, url.QueryEscape(db.SSLMode))
}
