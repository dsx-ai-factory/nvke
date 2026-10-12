// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package inspect reports which infrastructure providers a site controller can
// actually reach.
//
// A provider is usable when three things hold: its CRDs are established, its
// manager is running, and — unless it runs unauthenticated by design — its
// credentials Secret exists. Inspect answers all three from the cluster and
// says which of them is missing.
//
// A provider that is not installed is not an error. Reporting a site that has
// one provider, both, or neither is the point, so absence is a state rather
// than a failure.
package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/providers"
)

// defaultTimeout bounds the whole inspection. Without it an unreachable API
// server leaves kubectl waiting on a connection that will not open, and the
// command never returns.
const defaultTimeout = 30 * time.Second

// Runner executes a command and returns its combined output. It is the seam
// that lets tests assert on the kubectl invocations rather than run them.
//
// The signature matches controllerdeploy.CommandRunner, so controllerdeploy.ExecCommandRunner
// satisfies it without this package importing controllerdeploy.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (output string, err error)
}

// State is how far a provider has got towards being usable.
type State string

const (
	// StateNotInstalled means the provider's CRDs are absent, or present but
	// not established or serving a version.
	StateNotInstalled State = "NOT-INSTALLED"

	// StateNotConfigured means the CRDs are established but no credentials
	// Secret was found. For a provider that runs unauthenticated by design
	// this is a configuration choice rather than a fault.
	StateNotConfigured State = "NOT-CONFIGURED"

	// StateDegraded means the provider is configured but its manager is not
	// available — not running, not found, or not caught up with its own spec.
	StateDegraded State = "DEGRADED"

	// StateConfigured means the CRDs are established, the manager is
	// available, and credentials are in place.
	//
	// It does not mean the credentials work. Nothing here authenticates
	// against the provider's backend; only writing them, or creating a
	// cluster, proves that.
	StateConfigured State = "CONFIGURED"

	// StateUnknown means a probe failed for a reason that is not absence —
	// most often the kubeconfig cannot read Secrets in the manager's
	// namespace. The provider may be fine; nvkectl cannot tell.
	StateUnknown State = "UNKNOWN"
)

// Status is what Inspect found out about one provider.
type Status struct {
	Type                providers.Type `json:"type"`
	State               State          `json:"state"`
	CRDsEstablished     bool           `json:"crdsEstablished"`
	SecretPresent       bool           `json:"secretPresent"`
	SecretNamespace     string         `json:"secretNamespace,omitempty"`
	SecretName          string         `json:"secretName,omitempty"`
	ControllerAvailable bool           `json:"controllerAvailable"`
	ReadyReplicas       int            `json:"readyReplicas"`
	DesiredReplicas     int            `json:"desiredReplicas"`
	Detail              string         `json:"detail,omitempty"`
}

// Inspect reports the state of every registered provider in the cluster that
// kubeconfig points at.
//
// It returns an error only when nothing could be established at all: an
// unreadable kubeconfig, a missing kubectl, or an unreachable cluster. A
// provider that is absent, unconfigured or unreadable is reported in its
// Status, so one bad provider does not hide the others.
func Inspect(ctx context.Context, r Runner, kubeconfig string) ([]Status, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("no kubeconfig configured for this site: run 'nvkectl config set-site --kubeconfig <path>'")
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		return nil, fmt.Errorf("reading kubeconfig %s: %w (set it with 'nvkectl config set-site --kubeconfig <path>')", kubeconfig, err)
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	crds, err := establishedCRDs(ctx, r, kubeconfig)
	if err != nil {
		return nil, err
	}
	deployments, err := managerDeployments(ctx, r, kubeconfig)
	if err != nil {
		return nil, err
	}

	all := providers.Supported()
	out := make([]Status, 0, len(all))
	for _, d := range all {
		out = append(out, inspectOne(ctx, r, kubeconfig, d, crds, deployments))
	}
	return out, nil
}

func inspectOne(
	ctx context.Context,
	r Runner,
	kubeconfig string,
	d providers.Provider,
	crds map[string]bool,
	deployments []deployment,
) Status {
	s := Status{Type: d.Type}

	for _, name := range d.CRDNames {
		if !crds[name] {
			s.State = StateNotInstalled
			s.Detail = "no established CRD " + name
			return s
		}
	}
	s.CRDsEstablished = true

	mgr, found := findManager(d, deployments)
	if found {
		s.ControllerAvailable = mgr.available
		s.ReadyReplicas, s.DesiredReplicas = mgr.ready, mgr.desired
	}

	ns, name := credentialsSecret(d, mgr, found)
	s.SecretNamespace, s.SecretName = ns, name

	// A provider with no Secret name to probe cannot be configured through
	// one. CAPLL reaches this when its manager is not running to be asked and
	// it has no default name of its own.
	if name == "" {
		s.State = StateNotConfigured
		s.Detail = unconfiguredDetail(d, "no credentials Secret name known")
		return s
	}

	present, err := secretPresent(ctx, r, kubeconfig, ns, name)
	if err != nil {
		s.State = StateUnknown
		s.Detail = fmt.Sprintf("reading secret %s/%s: %s", ns, name, err)
		return s
	}
	s.SecretPresent = present

	switch {
	case !present:
		s.State = StateNotConfigured
		s.Detail = unconfiguredDetail(d, fmt.Sprintf("no secret %s/%s", ns, name))
	case found && !endpointConfigured(d, mgr):
		// Credentials without an address to send them to. The provider runs
		// and reports this once per reconcile rather than failing at startup,
		// so it is invisible without looking at the manager's flags.
		s.State = StateDegraded
		s.Detail = fmt.Sprintf("manager %s/%s has no %s, so the provider has no address to call",
			mgr.namespace, mgr.name, d.EndpointFlag)
	case !found:
		s.State = StateDegraded
		s.Detail = fmt.Sprintf("no manager Deployment found (looked for app.kubernetes.io/name=%s, then %s/%s)",
			d.AppName, d.ControllerNamespace, d.ControllerDeployment)
	case !s.ControllerAvailable:
		s.State = StateDegraded
		s.Detail = fmt.Sprintf("manager %s/%s not available (%d/%d ready)", mgr.namespace, mgr.name, mgr.ready, mgr.desired)
	default:
		s.State = StateConfigured
	}
	return s
}

// endpointConfigured reports whether the manager was given the address of the
// backend it calls. Providers that carry their endpoint in the credentials
// Secret instead declare no flag, and are reported as configured on the
// strength of the Secret alone.
func endpointConfigured(d providers.Provider, mgr deployment) bool {
	if d.EndpointFlag == "" {
		return true
	}
	v, ok := flagValue(mgr.args, d.EndpointFlag)
	return ok && v != ""
}

// unconfiguredDetail explains a missing Secret, noting when running without one
// is a supported configuration rather than something to fix.
func unconfiguredDetail(d providers.Provider, reason string) string {
	if d.UnconfiguredIsValid {
		return reason + "; this provider runs unauthenticated without one"
	}
	return reason
}

// credentialsSecret decides where the provider's credentials Secret should be.
//
// The manager is the authority: its namespace is where the Secret lives by
// default, and its own flags can name both. Only when no manager was found do
// the registry's defaults apply, and for a provider with no default name there
// is nothing to probe.
func credentialsSecret(d providers.Provider, mgr deployment, found bool) (namespace, name string) {
	if !found {
		return d.ControllerNamespace, d.DefaultSecretName
	}
	namespace, name = mgr.namespace, d.DefaultSecretName
	if v, ok := flagValue(mgr.args, d.SecretNamespaceFlag); ok {
		namespace = v
	}
	if v, ok := flagValue(mgr.args, d.SecretNameFlag); ok {
		name = v
	}
	return namespace, name
}

// flagValue finds flag in args, accepting both "--flag=value" and
// "--flag value".
func flagValue(args []string, flag string) (string, bool) {
	if flag == "" {
		return "", false
	}
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v, true
		}
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// deployment is the part of a manager Deployment this package reads.
type deployment struct {
	name      string
	namespace string
	appName   string
	args      []string
	available bool
	ready     int
	desired   int
}

// findManager locates d's manager, preferring the app.kubernetes.io/name label
// and falling back to the registry's namespace and name. The fallback matters
// because a kustomize namePrefix or a Helm release name changes the Deployment
// name without changing the label.
func findManager(d providers.Provider, deployments []deployment) (deployment, bool) {
	for _, dep := range deployments {
		if dep.appName == d.AppName {
			return dep, true
		}
	}
	for _, dep := range deployments {
		if dep.namespace == d.ControllerNamespace && dep.name == d.ControllerDeployment {
			return dep, true
		}
	}
	return deployment{}, false
}

// establishedCRDs returns the names of CRDs that are established and serving at
// least one version. A CRD that exists but is terminating, or has no served
// version, is not usable and is left out.
func establishedCRDs(ctx context.Context, r Runner, kubeconfig string) (map[string]bool, error) {
	out, err := r.Run(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "crd", "-o", "json")
	if err != nil {
		return nil, clusterError("listing CRDs", out, err)
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Versions []struct {
					Served bool `json:"served"`
				} `json:"versions"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing CRD list: %w", err)
	}

	established := make(map[string]bool, len(list.Items))
	for _, item := range list.Items {
		served := false
		for _, v := range item.Spec.Versions {
			if v.Served {
				served = true
				break
			}
		}
		if !served {
			continue
		}
		for _, c := range item.Status.Conditions {
			if c.Type == "Established" && c.Status == "True" {
				established[item.Metadata.Name] = true
				break
			}
		}
	}
	return established, nil
}

// managerDeployments lists every Deployment in the cluster that carries an
// app.kubernetes.io/name label, which is the set the providers' managers are
// drawn from.
func managerDeployments(ctx context.Context, r Runner, kubeconfig string) ([]deployment, error) {
	out, err := r.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "deployments", "--all-namespaces", "-l", "app.kubernetes.io/name", "-o", "json")
	if err != nil {
		return nil, clusterError("listing manager deployments", out, err)
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name       string            `json:"name"`
				Namespace  string            `json:"namespace"`
				Labels     map[string]string `json:"labels"`
				Generation int64             `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Args []string `json:"args"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				ReadyReplicas      int   `json:"readyReplicas"`
				Replicas           int   `json:"replicas"`
				Conditions         []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing deployment list: %w", err)
	}

	out2 := make([]deployment, 0, len(list.Items))
	for _, item := range list.Items {
		var args []string
		for _, c := range item.Spec.Template.Spec.Containers {
			args = append(args, c.Args...)
		}

		// Availability is asserted about the spec the controller has actually
		// observed. A Deployment mid-rollout can report ready replicas from the
		// previous generation while the new one is crash-looping.
		available := item.Status.ObservedGeneration >= item.Metadata.Generation
		if available {
			available = false
			for _, c := range item.Status.Conditions {
				if c.Type == "Available" && c.Status == "True" {
					available = true
					break
				}
			}
		}

		out2 = append(out2, deployment{
			name:      item.Metadata.Name,
			namespace: item.Metadata.Namespace,
			appName:   item.Metadata.Labels["app.kubernetes.io/name"],
			args:      args,
			available: available,
			ready:     item.Status.ReadyReplicas,
			desired:   item.Status.Replicas,
		})
	}
	return out2, nil
}

// secretPresent reports whether the named Secret exists. --ignore-not-found
// makes absence an empty success, so any error is a real failure to look.
func secretPresent(ctx context.Context, r Runner, kubeconfig, namespace, name string) (bool, error) {
	out, err := r.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"-n", namespace, "get", "secret", name, "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, firstLine(out)
	}
	return strings.TrimSpace(out) != "", nil
}

// clusterError turns a failed kubectl call into something that names the
// restriction rather than the symptom. A kubeconfig scoped to one namespace
// cannot list cluster-scoped resources, and that reads nothing like an outage.
func clusterError(what, out string, err error) error {
	lower := strings.ToLower(out)
	switch {
	case strings.Contains(lower, "forbidden"):
		return fmt.Errorf("%s: the site kubeconfig may not list CRDs and Deployments cluster-wide, "+
			"which this command needs: %w\n%s", what, err, strings.TrimSpace(out))
	case strings.Contains(lower, "executable file not found"):
		return fmt.Errorf("%s: kubectl was not found on PATH; nvkectl needs it to read the site controller: %w", what, err)
	default:
		return fmt.Errorf("%s: %w\n%s", what, err, strings.TrimSpace(out))
	}
}

// firstLine reduces kubectl's output to something fit for a table cell.
func firstLine(out string) error {
	trimmed := strings.TrimSpace(out)
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
		trimmed = trimmed[:i]
	}
	if trimmed == "" {
		trimmed = "kubectl failed"
	}
	return fmt.Errorf("%s", trimmed)
}
