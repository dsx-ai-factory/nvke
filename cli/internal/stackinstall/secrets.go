// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The Secret names and keys the values files reference.
const (
	databaseSecret = "nvke-database"
	tenantSecret   = "dgxc-tenant-credentials"
	nicoSecret     = "nico-credentials"
	spireCASecret  = "spire-upstream-ca"

	postgresPasswordKey = "postgres-password"
	connectionStringKey = "database-connection-string"
)

const (
	postgresNamespace   = "postgres"
	temporalNamespace   = "temporal"
	controllerNamespace = "nvke-controller"
	capnicoNamespace    = "capnico-system"
	spireNamespace      = "spire-server"
)

// The NVKE controller's database, as values/controller.yaml appDb records it.
const (
	dbHost    = "nvke-postgresql.postgres.svc.cluster.local:5432"
	dbName    = "nvke-controller"
	dbUser    = "postgres"
	dbSSLMode = "disable"
)

// NICoAPI is the NICo API that CAPNICo calls.
type NICoAPI struct {
	Endpoint string
	OrgID    string
	APIName  string
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
		return nil, fmt.Errorf("stackinstall: Credentials.SpireCACert and SpireCAKey must be set together")
	}
	password := []byte(c.DatabasePassword)
	secrets := []*corev1.Secret{
		secret(postgresNamespace, databaseSecret, map[string][]byte{postgresPasswordKey: password}),
		secret(temporalNamespace, databaseSecret, map[string][]byte{postgresPasswordKey: password}),
		secret(controllerNamespace, databaseSecret, map[string][]byte{connectionStringKey: []byte(connectionString(c.DatabasePassword))}),
		secret(controllerNamespace, tenantSecret, map[string][]byte{"user": []byte(c.NodeUser), "password": []byte(c.NodePassword)}),
		secret(capnicoNamespace, nicoSecret, map[string][]byte{
			"endpoint": []byte(nico.Endpoint), "orgID": []byte(nico.OrgID), "apiName": []byte(nico.APIName), "token": []byte(c.NicoToken),
		}),
	}
	if len(c.SpireCACert) > 0 {
		// A self-signed root is its own bundle.
		secrets = append(secrets, secret(spireNamespace, spireCASecret, map[string][]byte{
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

func connectionString(password string) string {
	return fmt.Sprintf("postgresql://%s@%s/%s?sslmode=%s", url.UserPassword(dbUser, password).String(), dbHost, dbName, url.QueryEscape(dbSSLMode))
}
