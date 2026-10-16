// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// example is the operator's starting point, read the way the deploy suite
// reads it: by its path in the checkout.
var example = filepath.Join("..", "..", "..", "..", "deploy", "examples", "stack-config.json")

// filled returns the example with every placeholder replaced by a value.
func filled(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(
		"REPLACE_NICO_ENDPOINT", "https://nico.example.com",
		"REPLACE_NICO_ORG_ID", "org-1",
		"REPLACE_NICO_API_NAME", "forge",
	).Replace(string(text))
}

// write puts body at a stack-config.json of its own and returns the path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stack-config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	t.Run("filled example loads", func(t *testing.T) {
		path := write(t, filled(t))
		c, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Nico.Endpoint != "https://nico.example.com" || c.Nico.OrgID != "org-1" || c.Nico.APIName != "forge" || c.Images != (Images{}) || c.Mirror != "" {
			t.Errorf("loaded: %+v", c)
		}
		if c.Path() != path {
			t.Errorf("Path: %s, want %s", c.Path(), path)
		}
	})
	t.Run("the example lists every placeholder", func(t *testing.T) {
		_, err := Load(example)
		if err == nil {
			t.Fatal("the example loaded with its placeholders")
		}
		want := []string{
			"nico.endpoint: still a REPLACE_* placeholder; fill it in",
			"nico.orgID: still a REPLACE_* placeholder; fill it in",
			"nico.apiName: still a REPLACE_* placeholder; fill it in",
		}
		if err.Error() != strings.Join(want, "\n") {
			t.Errorf("placeholders:\n got %q\nwant %q", err.Error(), strings.Join(want, "\n"))
		}
	})
	t.Run("optional values are read", func(t *testing.T) {
		body := strings.NewReplacer(
			`"controller": ""`, `"controller": "registry.example.com/nvke/cwe:v1"`,
			`"profiles": ""`, `"profiles": "registry.example.com/nvke/profiles@sha256:`+strings.Repeat("a", 64)+`"`,
			`"mirror": ""`, `"mirror": "mirror.example.com:5000"`,
		).Replace(filled(t))
		c, err := Load(write(t, body))
		if err != nil {
			t.Fatal(err)
		}
		if c.Images.Controller != "registry.example.com/nvke/cwe:v1" || !strings.HasPrefix(c.Images.Profiles, "registry.example.com/nvke/profiles@sha256:") || c.Mirror != "mirror.example.com:5000" {
			t.Errorf("optional values: %+v", c)
		}
	})
	t.Run("a Config that was not loaded has no path", func(t *testing.T) {
		var c Config
		if c.Path() != "" {
			t.Errorf("Path on a zero Config: %q", c.Path())
		}
	})
}

func TestLoad_Refusals(t *testing.T) {
	long := strings.Repeat("a", 40)
	cases := map[string]struct {
		// edit rewrites the filled body; body replaces it outright.
		edit func(body string) string
		body string
		// want is a line the error must carry; absent is text it must not.
		want   string
		absent string
	}{
		"empty required value": {
			edit: func(b string) string { return strings.Replace(b, `"orgID": "org-1"`, `"orgID": ""`, 1) },
			want: "nico.orgID: must be set",
		},
		"unknown key at the top level": {
			edit: func(b string) string { return strings.Replace(b, `"mirror": ""`, `"mirror": "", "arch": "arm64"`, 1) },
			want: "arch: unknown field",
		},
		"unknown key in an object": {
			edit: func(b string) string { return strings.Replace(b, `"endpoint":`, `"endpont":`, 1) },
			want: "nico.endpont: unknown field",
		},
		"duplicate key": {
			edit: func(b string) string {
				return strings.Replace(b, `"orgID": "org-1",`, `"orgID": "org-1", "orgID": "org-2",`, 1)
			},
			want: "nico.orgID: duplicate field",
		},
		"key with the wrong case": {
			edit: func(b string) string { return strings.Replace(b, `"nico":`, `"Nico":`, 1) },
			want: "Nico: unknown field",
		},
		"non-object top level": {
			body: `["nico"]`,
			want: "the file does not hold a JSON object",
		},
		"empty file": {
			body: " \n",
			want: "the file is empty",
		},
		"trailing content": {
			edit: func(b string) string { return b + "{}\n" },
			want: "after top-level value",
		},
		"null nico": {
			edit: func(b string) string {
				return b[:strings.Index(b, `"nico"`)] + `"nico": null,` + b[strings.Index(b, `"images"`):]
			},
			want: "nico: must be an object, not null",
		},
		"null images": {
			edit: func(b string) string {
				return b[:strings.Index(b, `"images"`)] + `"images": null,` + b[strings.Index(b, `"mirror"`):]
			},
			want: "images: must be an object, not null",
		},
		"wrong type": {
			edit: func(b string) string {
				return b[:strings.Index(b, `"images"`)] + `"images": "x",` + b[strings.Index(b, `"mirror"`):]
			},
			want: "images",
		},
		"endpoint without a scheme": {
			edit: func(b string) string { return strings.Replace(b, "https://nico.example.com", "nico.example.com", 1) },
			want: `nico.endpoint: "nico.example.com" must be an http or https URL`,
		},
		"endpoint with a user and password": {
			edit: func(b string) string {
				return strings.Replace(b, "https://nico.example.com", "https://nico:"+long+"@nico.example.com", 1)
			},
			want:   "nico.endpoint: must carry no user or password; the token comes from NVKECTL_NICO_TOKEN",
			absent: long,
		},
		"a quote in a template value": {
			edit: func(b string) string { return strings.Replace(b, `"org-1"`, `"org\"1"`, 1) },
			want: "nico.orgID: the value holds a character the templates cannot carry",
		},
		"mirror with a scheme": {
			edit: func(b string) string {
				return strings.Replace(b, `"mirror": ""`, `"mirror": "https://mirror.example.com"`, 1)
			},
			want: `mirror: "https://mirror.example.com" is a host[:port][/prefix], not a URL`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := tc.body
			if tc.edit != nil {
				body = tc.edit(filled(t))
			}
			_, err := Load(write(t, body))
			if err == nil {
				t.Fatalf("accepted:\n%s", body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, err)
			}
			if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
				t.Errorf("the error carries the value:\n%s", err)
			}
			for _, line := range strings.Split(err.Error(), "\n") {
				if strings.HasPrefix(line, "the file ") || strings.Contains(line, "after top-level value") || strings.HasPrefix(line, "cannot unmarshal ") {
					continue
				}
				if _, _, ok := strings.Cut(line, ": "); !ok {
					t.Errorf("line is not <json path>: <problem>: %q", line)
				}
			}
		})
	}
}

// Every problem is reported at once, so a file is fixed in one pass: the
// value problems, and a stray key beside them.
func TestLoad_ReportsEveryProblem(t *testing.T) {
	body := filled(t)
	body = strings.Replace(body, `"orgID": "org-1"`, `"orgID": ""`, 1)
	body = strings.Replace(body, "https://nico.example.com", "nico.example.com", 1)
	body = strings.Replace(body, `"mirror": ""`, `"mirror": "https://m"`, 1)
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("accepted")
	}
	if lines := strings.Split(err.Error(), "\n"); len(lines) != 3 {
		t.Errorf("want three lines, got %d:\n%s", len(lines), err)
	}
	for _, want := range []string{"nico.orgID: must be set", "nico.endpoint:", "mirror:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in:\n%s", want, err)
		}
	}
	body = strings.Replace(body, `"mirror": "https://m"`, `"mirror": "", "pullSecret": "x"`, 1)
	_, err = Load(write(t, body))
	if err == nil {
		t.Fatal("accepted")
	}
	if lines := strings.Split(err.Error(), "\n"); len(lines) != 3 || lines[0] != "pullSecret: unknown field" || !strings.Contains(err.Error(), "nico.orgID: must be set") {
		t.Errorf("a stray key and the value problems in one pass:\n%s", err)
	}
	// A null object is the one problem reported for it: its fields are not
	// listed as missing too.
	body = filled(t)
	body = body[:strings.Index(body, `"nico"`)] + `"nico": null,` + body[strings.Index(body, `"images"`):]
	_, err = Load(write(t, body))
	if err == nil || !strings.Contains(err.Error(), "nico: must be an object, not null") || strings.Contains(err.Error(), "nico.endpoint") {
		t.Errorf("null object:\n%v", err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil || !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("missing file: %v", err)
	}
}

func TestCredentialsFromEnv(t *testing.T) {
	t.Setenv(EnvNicoToken, " token\n")
	t.Setenv(EnvNodeUser, "nke")
	t.Setenv(EnvNodePassword, "")
	t.Setenv(EnvDatabasePassword, "db")
	creds, err := CredentialsFromEnv()
	if err == nil || !strings.Contains(err.Error(), EnvNodePassword+" is not set") {
		t.Errorf("unset password: %v", err)
	}
	if creds.NicoToken != "token" || creds.NodeUser != "nke" || creds.DatabasePassword != "db" {
		t.Errorf("secrets not read and trimmed: %+v", creds)
	}
	t.Setenv(EnvNodePassword, "pw")
	if _, err := CredentialsFromEnv(); err != nil {
		t.Errorf("all set: %v", err)
	}
	// The templated credentials refuse what the templates cannot carry; the
	// database password, written through YAML, does not.
	t.Setenv(EnvDatabasePassword, `d"b\`)
	if _, err := CredentialsFromEnv(); err != nil {
		t.Errorf("database password with a quote: %v", err)
	}
	for _, name := range []string{EnvNicoToken, EnvNodeUser, EnvNodePassword} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, `a"b`)
			_, err := CredentialsFromEnv()
			if err == nil || !strings.Contains(err.Error(), name+": the value holds a character") || strings.Contains(err.Error(), `a"b`) {
				t.Errorf("quote in %s: %v", name, err)
			}
		})
	}
	// Every unset variable is named at once.
	for _, name := range []string{EnvNicoToken, EnvNodeUser, EnvNodePassword, EnvDatabasePassword} {
		t.Setenv(name, "")
	}
	_, err = CredentialsFromEnv()
	if err == nil || strings.Count(err.Error(), " is not set") != 4 {
		t.Errorf("four unset variables: %v", err)
	}
}
