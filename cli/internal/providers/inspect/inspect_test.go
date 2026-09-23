// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package inspect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/cli/internal/providers"
)

// fakeRunner answers kubectl calls by matching on the arguments, not on the
// order they arrive in. Dispatching by call index would let a test pass while
// the code under test asked the cluster an entirely different question.
type fakeRunner struct {
	// responses maps a substring of the joined args to the output returned.
	responses map[string]string
	// failures maps the same to an error, with the output returned alongside.
	failures map[string]string
	calls    [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	joined := strings.Join(args, " ")
	for match, out := range f.failures {
		if strings.Contains(joined, match) {
			return out, errors.New("exit status 1")
		}
	}
	for match, out := range f.responses {
		if strings.Contains(joined, match) {
			return out, nil
		}
	}
	return "", errors.New("fakeRunner: no scripted response for: " + joined)
}

// called reports whether any recorded invocation contains every fragment.
func (f *fakeRunner) called(fragments ...string) bool {
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		all := true
		for _, frag := range fragments {
			if !strings.Contains(joined, frag) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// supported returns the entry for typ from the set nkectl supports.
func supported(t *testing.T, typ providers.Type) providers.Provider {
	t.Helper()
	for _, p := range providers.Supported() {
		if p.Type == typ {
			return p
		}
	}
	t.Fatalf("%s is not in the supported set", typ)
	return providers.Provider{}
}

// kubeconfig writes a throwaway file so Inspect's existence check passes.
func kubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

// crdJSON renders a CRD list where every named CRD is established and serving.
func crdJSON(names ...string) string {
	items := make([]string, 0, len(names))
	for _, n := range names {
		items = append(items, `{"metadata":{"name":"`+n+`"},`+
			`"spec":{"versions":[{"served":true}]},`+
			`"status":{"conditions":[{"type":"Established","status":"True"}]}}`)
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

const emptyList = `{"items":[]}`

// deployJSON renders one manager Deployment.
func deployJSON(namespace, name, appName string, available bool, ready, desired int, args string) string {
	cond := "False"
	if available {
		cond = "True"
	}
	return `{"items":[{"metadata":{"name":"` + name + `","namespace":"` + namespace + `",` +
		`"labels":{"app.kubernetes.io/name":"` + appName + `"},"generation":1},` +
		`"spec":{"template":{"spec":{"containers":[{"args":[` + args + `]}]}}},` +
		`"status":{"observedGeneration":1,"readyReplicas":` + itoa(ready) + `,"replicas":` + itoa(desired) +
		`,"conditions":[{"type":"Available","status":"` + cond + `"}]}}]}`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for ; i > 0; i /= 10 {
		digits = string(rune('0'+i%10)) + digits
	}
	return digits
}

// statusFor picks one provider's result out of the slice.
func statusFor(t *testing.T, all []Status, typ providers.Type) Status {
	t.Helper()
	for _, s := range all {
		if s.Type == typ {
			return s
		}
	}
	t.Fatalf("no status reported for %s", typ)
	return Status{}
}

func TestInspect_NoCRDsInstalled(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{
		"get crd":         emptyList,
		"get deployments": emptyList,
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect returned an error for a cluster with no providers: %v", err)
	}
	if len(got) != len(providers.Supported()) {
		t.Fatalf("reported %d providers, want %d", len(got), len(providers.Supported()))
	}
	for _, s := range got {
		if s.State != StateNotInstalled {
			t.Errorf("%s state = %s, want %s", s.Type, s.State, StateNotInstalled)
		}
	}
	if r.called("get secret") {
		t.Error("probed for a Secret even though no provider was installed")
	}
}

func TestInspect_OneInstalledOneNot(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": emptyList,
		"get secret":      "",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s := statusFor(t, got, providers.TypeNico); s.State != StateNotConfigured {
		t.Errorf("nico state = %s (%s), want %s", s.State, s.Detail, StateNotConfigured)
	}
	if s := statusFor(t, got, providers.TypeLaunchLayer); s.State != StateNotInstalled {
		t.Errorf("launchlayer state = %s, want %s", s.State, StateNotInstalled)
	}
}

func TestInspect_CRDNotEstablished(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	notEstablished := `{"items":[{"metadata":{"name":"` + nico.CRDNames[0] + `"},` +
		`"spec":{"versions":[{"served":true}]},` +
		`"status":{"conditions":[{"type":"Established","status":"False"}]}}]}`
	r := &fakeRunner{responses: map[string]string{
		"get crd":         notEstablished,
		"get deployments": emptyList,
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s := statusFor(t, got, providers.TypeNico); s.State != StateNotInstalled {
		t.Errorf("state = %s, want %s for a CRD that is not established", s.State, StateNotInstalled)
	}
}

func TestInspect_CRDNoServedVersion(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	unserved := `{"items":[{"metadata":{"name":"` + nico.CRDNames[0] + `"},` +
		`"spec":{"versions":[{"served":false}]},` +
		`"status":{"conditions":[{"type":"Established","status":"True"}]}}]}`
	r := &fakeRunner{responses: map[string]string{
		"get crd":         unserved,
		"get deployments": emptyList,
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s := statusFor(t, got, providers.TypeNico); s.State != StateNotInstalled {
		t.Errorf("state = %s, want %s for a CRD serving no version", s.State, StateNotInstalled)
	}
}

func TestInspect_ConfiguredWhenManagerAvailable(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": deployJSON("capnico-system", "controller-manager", nico.AppName, true, 1, 1, ""),
		"get secret":      "secret/nico-credentials\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	s := statusFor(t, got, providers.TypeNico)
	if s.State != StateConfigured {
		t.Errorf("state = %s (%s), want %s", s.State, s.Detail, StateConfigured)
	}
	if !r.called("-n", "capnico-system", "get secret", "nico-credentials") {
		t.Errorf("did not probe the expected Secret; calls were %v", r.calls)
	}
}

// TestInspect_NoManagerDeployment covers the state the end-to-end suite
// produces: CRDs applied and a Secret written, but no controller running.
func TestInspect_NoManagerDeployment(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": emptyList,
		"get secret":      "secret/nico-credentials\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	s := statusFor(t, got, providers.TypeNico)
	if s.State != StateDegraded {
		t.Errorf("state = %s, want %s when the manager is absent", s.State, StateDegraded)
	}
	if !strings.Contains(s.Detail, "no manager Deployment found") {
		t.Errorf("detail = %q, want it to say the manager was not found", s.Detail)
	}
}

func TestInspect_ManagerNotAvailable(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": deployJSON("capnico-system", "controller-manager", nico.AppName, false, 0, 1, ""),
		"get secret":      "secret/nico-credentials\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	s := statusFor(t, got, providers.TypeNico)
	if s.State != StateDegraded {
		t.Errorf("state = %s, want %s", s.State, StateDegraded)
	}
	if s.ReadyReplicas != 0 || s.DesiredReplicas != 1 {
		t.Errorf("replicas = %d/%d, want 0/1", s.ReadyReplicas, s.DesiredReplicas)
	}
}

// TestInspect_ManagerFlagsOverrideSecretLocation covers a manager told to read
// its credentials from somewhere other than the provider's default.
func TestInspect_ManagerFlagsOverrideSecretLocation(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	args := `"--provider-credentials-namespace=other-ns","--provider-credentials-secret-name","other-secret"`
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": deployJSON("capnico-system", "controller-manager", nico.AppName, true, 1, 1, args),
		"get secret":      "secret/other-secret\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	s := statusFor(t, got, providers.TypeNico)
	if s.SecretNamespace != "other-ns" || s.SecretName != "other-secret" {
		t.Errorf("secret = %s/%s, want other-ns/other-secret", s.SecretNamespace, s.SecretName)
	}
	if !r.called("-n", "other-ns", "get secret", "other-secret") {
		t.Errorf("did not probe the overridden Secret; calls were %v", r.calls)
	}
}

// TestInspect_ManagerFoundByNamespaceAndName covers a provider whose manager
// carries no recognisable app label, so only the registry's fallback finds it.
func TestInspect_ManagerFoundByNamespaceAndName(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": deployJSON("capnico-system", "controller-manager", "something-else", true, 1, 1, ""),
		"get secret":      "secret/nico-credentials\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s := statusFor(t, got, providers.TypeNico); s.State != StateConfigured {
		t.Errorf("state = %s (%s), want %s via the namespace/name fallback", s.State, s.Detail, StateConfigured)
	}
}

// TestInspect_SecretProbeForbidden covers a kubeconfig that may list providers
// but not read their credentials. That is not knowledge of a broken provider,
// and it must not fail the whole table.
func TestInspect_SecretProbeForbidden(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	r := &fakeRunner{
		responses: map[string]string{
			"get crd":         crdJSON(nico.CRDNames...),
			"get deployments": deployJSON("capnico-system", "controller-manager", nico.AppName, true, 1, 1, ""),
		},
		failures: map[string]string{
			"get secret": `Error from server (Forbidden): secrets "nico-credentials" is forbidden`,
		},
	}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect returned an error for an unreadable Secret: %v", err)
	}
	s := statusFor(t, got, providers.TypeNico)
	if s.State != StateUnknown {
		t.Errorf("state = %s, want %s", s.State, StateUnknown)
	}
	if !strings.Contains(s.Detail, "Forbidden") {
		t.Errorf("detail = %q, want it to carry the reason", s.Detail)
	}
	if s := statusFor(t, got, providers.TypeLaunchLayer); s.State != StateNotInstalled {
		t.Errorf("one unreadable provider changed another's state to %s", s.State)
	}
}

// TestInspect_LaunchLayerUnconfiguredIsNoted covers the provider that runs
// unauthenticated by design, where a missing Secret is a choice.
func TestInspect_LaunchLayerUnconfiguredIsNoted(t *testing.T) {
	ll := supported(t, providers.TypeLaunchLayer)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(ll.CRDNames...),
		"get deployments": emptyList,
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	s := statusFor(t, got, providers.TypeLaunchLayer)
	if s.State != StateNotConfigured {
		t.Errorf("state = %s, want %s", s.State, StateNotConfigured)
	}
	if !strings.Contains(s.Detail, "unauthenticated") {
		t.Errorf("detail = %q, want it to say running without a Secret is supported", s.Detail)
	}
}

// TestInspect_LaunchLayerWithoutBaseURL covers credentials with nowhere to send
// them. CAPLL takes its address from --base-url rather than the Secret, and an
// absent address is not a startup failure, so this is otherwise invisible.
func TestInspect_LaunchLayerWithoutBaseURL(t *testing.T) {
	ll := supported(t, providers.TypeLaunchLayer)
	args := `"--identity-secret=ll-credentials"`
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(ll.CRDNames...),
		"get deployments": deployJSON("capll-system", "mgr", ll.AppName, true, 1, 1, args),
		"get secret":      "secret/ll-credentials\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	s := statusFor(t, got, providers.TypeLaunchLayer)
	if s.State != StateDegraded {
		t.Errorf("state = %s (%s), want %s when the manager has no --base-url", s.State, s.Detail, StateDegraded)
	}
	if !strings.Contains(s.Detail, "--base-url") {
		t.Errorf("detail = %q, want it to name the missing flag", s.Detail)
	}
}

func TestInspect_LaunchLayerWithBaseURLIsConfigured(t *testing.T) {
	ll := supported(t, providers.TypeLaunchLayer)
	args := `"--identity-secret=ll-credentials","--base-url","https://ll.example.com"`
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(ll.CRDNames...),
		"get deployments": deployJSON("capll-system", "mgr", ll.AppName, true, 1, 1, args),
		"get secret":      "secret/ll-credentials\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s := statusFor(t, got, providers.TypeLaunchLayer); s.State != StateConfigured {
		t.Errorf("state = %s (%s), want %s", s.State, s.Detail, StateConfigured)
	}
}

// TestInspect_NicoNeedsNoEndpointFlag guards the asymmetry: NICo carries its
// endpoint in the credentials Secret, so it must not be judged on manager flags.
func TestInspect_NicoNeedsNoEndpointFlag(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": deployJSON("capnico-system", "controller-manager", nico.AppName, true, 1, 1, ""),
		"get secret":      "secret/nico-credentials\n",
	}}

	got, err := Inspect(context.Background(), r, kubeconfig(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s := statusFor(t, got, providers.TypeNico); s.State != StateConfigured {
		t.Errorf("state = %s (%s), want %s with no endpoint flag on the manager", s.State, s.Detail, StateConfigured)
	}
}

func TestInspect_KubeconfigNotConfigured(t *testing.T) {
	r := &fakeRunner{}
	_, err := Inspect(context.Background(), r, "")
	if err == nil {
		t.Fatal("Inspect accepted an empty kubeconfig")
	}
	if !strings.Contains(err.Error(), "config set-site") {
		t.Errorf("error = %q, want it to name the command that fixes it", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("ran %d commands without a kubeconfig", len(r.calls))
	}
}

func TestInspect_KubeconfigMissingFile(t *testing.T) {
	r := &fakeRunner{}
	missing := filepath.Join(t.TempDir(), "absent.kubeconfig")
	_, err := Inspect(context.Background(), r, missing)
	if err == nil {
		t.Fatal("Inspect accepted a kubeconfig path that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %q, want it to name the path", err)
	}
}

func TestInspect_ForbiddenListNamesThePermission(t *testing.T) {
	r := &fakeRunner{failures: map[string]string{
		"get crd": `Error from server (Forbidden): customresourcedefinitions is forbidden`,
	}}

	_, err := Inspect(context.Background(), r, kubeconfig(t))
	if err == nil {
		t.Fatal("Inspect succeeded despite being unable to list CRDs")
	}
	if !strings.Contains(err.Error(), "cluster-wide") {
		t.Errorf("error = %q, want it to explain the permission it needs", err)
	}
}

// TestInspect_ArgvIsExact pins the questions asked of the cluster. The
// arguments are the whole behaviour of a command that shells out, so a change
// here should be deliberate.
func TestInspect_ArgvIsExact(t *testing.T) {
	nico := supported(t, providers.TypeNico)
	kc := kubeconfig(t)
	r := &fakeRunner{responses: map[string]string{
		"get crd":         crdJSON(nico.CRDNames...),
		"get deployments": deployJSON("capnico-system", "controller-manager", nico.AppName, true, 1, 1, ""),
		"get secret":      "secret/nico-credentials\n",
	}}

	if _, err := Inspect(context.Background(), r, kc); err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	want := [][]string{
		{"kubectl", "--kubeconfig", kc, "get", "crd", "-o", "json"},
		{"kubectl", "--kubeconfig", kc, "get", "deployments", "--all-namespaces", "-l", "app.kubernetes.io/name", "-o", "json"},
		{"kubectl", "--kubeconfig", kc, "-n", "capnico-system", "get", "secret", "nico-credentials", "--ignore-not-found", "-o", "name"},
	}
	for _, w := range want {
		found := false
		for _, c := range r.calls {
			if strings.Join(c, "\x00") == strings.Join(w, "\x00") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected invocation not made: %v\ngot: %v", w, r.calls)
		}
	}
}

func TestFlagValue(t *testing.T) {
	cases := []struct {
		name string
		args []string
		flag string
		want string
		ok   bool
	}{
		{"equals form", []string{"--identity-secret=creds"}, "--identity-secret", "creds", true},
		{"space form", []string{"--identity-secret", "creds"}, "--identity-secret", "creds", true},
		{"absent", []string{"--other=1"}, "--identity-secret", "", false},
		{"empty flag name", []string{"--identity-secret=creds"}, "", "", false},
		{"trailing flag with no value", []string{"--identity-secret"}, "--identity-secret", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := flagValue(tc.args, tc.flag)
			if got != tc.want || ok != tc.ok {
				t.Errorf("flagValue() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
