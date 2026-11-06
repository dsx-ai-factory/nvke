// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// EnvControllerSPIFFEID is the SPIFFE ID SPIRE on the bootstrap cluster should
// issue the controller. When set, TestE2E_BootstrapSite checks the controller
// there with VerifyControllerIdentity once the bootstrap succeeds, which also
// needs EnvBootstrapCluster and EnvControllerName.
const EnvControllerSPIFFEID = "NVKECTL_E2E_CONTROLLER_SPIFFE_ID"

// The SPIRE server bootstrap site installs.
const (
	// SpireNamespace is where the NVKE stack installs SPIRE.
	SpireNamespace       = "spire-server"
	spireServerPod       = "spire-server-0"
	spireServerContainer = "spire-server"
	spireServerBinary    = "/opt/spire/bin/spire-server"
)

// ControllerIdentity is what VerifyControllerIdentity expects of the
// controller.
type ControllerIdentity struct {
	// Name is the controller's Deployment, its namespace and its
	// ServiceAccount.
	Name string
	// SPIFFEID is the ID SPIRE should hold an entry for.
	SPIFFEID string
	// UpstreamCA is the PEM CA SPIRE's bundle should hold, or nil when the run
	// supplied none.
	UpstreamCA []byte
}

// SpireServerCLI runs spire-server with args in the SPIRE server and returns
// its standard output.
type SpireServerCLI func(ctx context.Context, args ...string) (string, error)

// ControllerIdentityFromEnv returns the identity EnvControllerSPIFFEID asks
// for, with an empty SPIFFEID when it is unset. It reads the upstream CA from
// the file sitespec.EnvSpireCACertFile names, as bootstrap site does, and
// fails when a variable the check needs is missing or the CA cannot be read.
func ControllerIdentityFromEnv() (ControllerIdentity, error) {
	spiffeID := os.Getenv(EnvControllerSPIFFEID)
	if spiffeID == "" {
		return ControllerIdentity{}, nil
	}
	name := os.Getenv(EnvControllerName)
	switch {
	case os.Getenv(EnvBootstrapCluster) == "":
		return ControllerIdentity{}, fmt.Errorf("%s is set but %s is not: name the bootstrap cluster", EnvControllerSPIFFEID, EnvBootstrapCluster)
	case name == "":
		return ControllerIdentity{}, fmt.Errorf("%s is set but %s is not: name the controller", EnvControllerSPIFFEID, EnvControllerName)
	}
	want := ControllerIdentity{Name: name, SPIFFEID: spiffeID}
	if caFile := os.Getenv(sitespec.EnvSpireCACertFile); caFile != "" {
		ca, err := os.ReadFile(caFile)
		if err != nil {
			return ControllerIdentity{}, fmt.Errorf("reading the SPIRE upstream CA %s: %w", caFile, err)
		}
		want.UpstreamCA = ca
	}
	return want, nil
}

// VerifyControllerIdentity checks, once, that the controller's Deployment runs
// as the ServiceAccount named after it, its external Service has a ready
// endpoint, SPIRE holds an entry for want.SPIFFEID, and, with
// want.UpstreamCA, that SPIRE's bundle holds that CA. It does not wait: it is
// called after bootstrap site has reported the controller healthy. The error
// names every problem found.
func VerifyControllerIdentity(ctx context.Context, kube kubernetes.Interface, spire SpireServerCLI, want ControllerIdentity) error {
	var problems []string
	d, err := kube.AppsV1().Deployments(want.Name).Get(ctx, want.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading deployment %s/%s: %w", want.Name, want.Name, err)
	}
	if sa := d.Spec.Template.Spec.ServiceAccountName; sa != want.Name {
		problems = append(problems, fmt.Sprintf("deployment %s/%s runs as ServiceAccount %s, not %s", want.Name, want.Name, sa, want.Name))
	}

	service := want.Name + "-external"
	ready, err := hasReadyEndpoint(ctx, kube, want.Name, service)
	if err != nil {
		return err
	}
	if !ready {
		problems = append(problems, fmt.Sprintf("Service %s/%s has no ready endpoint", want.Name, service))
	}

	entries, err := spire(ctx, "entry", "show", "-spiffeID", want.SPIFFEID)
	if err != nil {
		return fmt.Errorf("asking SPIRE for the entry of %s: %w", want.SPIFFEID, err)
	}
	if !hasEntry(entries) {
		problems = append(problems, "SPIRE has no entry for "+want.SPIFFEID)
	}

	if len(want.UpstreamCA) > 0 {
		bundle, err := spire(ctx, "bundle", "show")
		if err != nil {
			return fmt.Errorf("asking SPIRE for its bundle: %w", err)
		}
		holds, err := bundleHolds(bundle, want.UpstreamCA)
		if err != nil {
			return err
		}
		if !holds {
			problems = append(problems, "SPIRE's bundle does not hold the upstream CA")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("the controller on the bootstrap cluster does not run under its identity: %s", strings.Join(problems, "; "))
	}
	return nil
}

// SpireServerExec returns a SpireServerCLI that runs spire-server in the SPIRE
// server pod of the cluster config reaches, through kube.
func SpireServerExec(config *rest.Config, kube kubernetes.Interface) SpireServerCLI {
	return func(ctx context.Context, args ...string) (string, error) {
		request := kube.CoreV1().RESTClient().Post().
			Namespace(SpireNamespace).
			Resource("pods").
			Name(spireServerPod).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: spireServerContainer,
				Command:   append([]string{spireServerBinary}, args...),
				Stdout:    true,
				Stderr:    true,
			}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, request.URL())
		if err != nil {
			return "", fmt.Errorf("building the exec into %s/%s: %w", SpireNamespace, spireServerPod, err)
		}
		var stdout, stderr bytes.Buffer
		if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
			if message := strings.TrimSpace(stderr.String()); message != "" {
				return "", fmt.Errorf("running spire-server %s: %w: %s", strings.Join(args, " "), err, message)
			}
			return "", fmt.Errorf("running spire-server %s: %w", strings.Join(args, " "), err)
		}
		return stdout.String(), nil
	}
}

// hasReadyEndpoint reports whether any EndpointSlice of service holds a ready
// endpoint with an address.
func hasReadyEndpoint(ctx context.Context, kube kubernetes.Interface, ns, service string) (bool, error) {
	endpointSlices, err := kube.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + service,
	})
	if err != nil {
		return false, fmt.Errorf("listing the endpoints of Service %s/%s: %w", ns, service, err)
	}
	for _, slice := range endpointSlices.Items {
		for _, endpoint := range slice.Endpoints {
			// A nil Ready is ready, as the EndpointSlice API defines it.
			if len(endpoint.Addresses) > 0 && (endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready) {
				return true, nil
			}
		}
	}
	return false, nil
}

// hasEntry reports whether `spire-server entry show` printed an entry.
func hasEntry(output string) bool {
	for line := range strings.Lines(output) {
		if strings.HasPrefix(strings.TrimSpace(line), "Entry ID") {
			return true
		}
	}
	return false
}

// bundleHolds reports whether the PEM bundle holds the first certificate in
// ca.
func bundleHolds(bundle string, ca []byte) (bool, error) {
	want, _ := pem.Decode(ca)
	if want == nil || want.Type != "CERTIFICATE" {
		return false, errors.New("the SPIRE upstream CA holds no PEM certificate")
	}
	rest := []byte(bundle)
	for {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return false, nil
		}
		if block.Type == "CERTIFICATE" && bytes.Equal(block.Bytes, want.Bytes) {
			return true, nil
		}
	}
}
