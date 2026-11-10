// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestControllerValuesNameEveryObject pins the chart values that have to carry
// the name besides the fullname: nameOverride is the pod label the external
// Service selects on, and the chart pins serviceAccount.name instead of
// deriving it, which the SPIRE admin ID and the controller's SPIFFE ID name.
func TestControllerValuesNameEveryObject(t *testing.T) {
	body, err := fs.ReadFile(manifestsFS, cweValuesFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fullnameOverride: " + cweNamespace + "\n",
		"nameOverride: " + cweNamespace + "\n",
		"namespaceOverride: " + cweNamespace + "\n",
		"serviceAccount:\n  name: " + cweNamespace + "\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s does not set %q", cweValuesFile, strings.TrimSpace(want))
		}
	}
}

// TestPostgresCreatesTheDatabaseTheControllerUses pins the two settings that
// have to agree. The controller's database name is appDb.dbName in the
// controller values; the database is created by the Postgres script that
// postgres-values.yaml replaces. A name on one side only installs cleanly and
// leaves the controller unready, because it connects to a database nobody
// created. The Temporal script is not replaced, so the chart still creates the
// Temporal databases.
func TestPostgresCreatesTheDatabaseTheControllerUses(t *testing.T) {
	var controller struct {
		AppDB struct {
			DBName string `yaml:"dbName"`
		} `yaml:"appDb"`
	}
	var postgres struct {
		InitDB struct {
			Scripts map[string]string `yaml:"scripts"`
		} `yaml:"initdb"`
	}
	for file, into := range map[string]any{cweValuesFile: &controller, postgresValuesFile: &postgres} {
		body, err := fs.ReadFile(manifestsFS, file)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(body, into); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}

	if controller.AppDB.DBName != cweNamespace {
		t.Errorf("%s appDb.dbName = %q, want %q", cweValuesFile, controller.AppDB.DBName, cweNamespace)
	}
	if got, want := postgres.InitDB.Scripts["init-cwe.sql"], `CREATE DATABASE "`+controller.AppDB.DBName+`";`; strings.TrimSpace(got) != want {
		t.Errorf("%s init-cwe.sql = %q, want %q", postgresValuesFile, got, want)
	}
	if len(postgres.InitDB.Scripts) != 1 {
		t.Errorf("%s sets scripts %v, want only init-cwe.sql: the Temporal script stays as the chart ships it", postgresValuesFile, postgres.InitDB.Scripts)
	}
}
