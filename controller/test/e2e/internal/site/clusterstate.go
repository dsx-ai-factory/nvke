// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

// sectionMarker prefixes each command's output so the sections stay legible
// when the file is read whole, which is how it is read.
const (
	sectionMarker = "===== "
)

// CollectClusterState asks the site cluster what it makes of the controller, and writes
// the answer to dest.
//
// This exists because waitForCWE reports only that nothing answered. Runs
// 37454470331 attempt 1 and attempt 2 both failed as
//
//	controller did not become healthy within 10m0s: GET /healthz through
//	port-forward to ...:8080: context deadline exceeded
//
// and that is the whole of what either run recorded. nvkectl's own diagnostics
// do not cover this: it writes them when the bootstrap fails, and the
// bootstrap had succeeded, so CollectDiagnostics returned ErrNoDiagnostics
// and the runs produced no cluster-side evidence at all.
//
// The probe reaches the controller over a port-forward from the runner, so a timeout has
// two quite different causes and the error text is identical for both: the controller is
// not ready, or the controller is ready and the path to it is not. Endpoints is what
// separates them, which is why it is collected first and why endpointslices
// are collected beside it -- a Service with a ready endpoint and a probe that
// times out is a path fault, and raising the timeout would not touch it.
//
// Everything here runs on the node against the kubeconfig nvkectl wrote, so it
// needs no forward of its own. That matters: if the forward is what broke,
// a collector that depended on one would fail in the same way as the probe
// and report nothing.
func CollectClusterState(ctx context.Context, s infra.Session, dest string, secrets ...string) error {
	out, err := s.Run(ctx, asRoot(clusterStateScript()))
	// Not a failure to report upwards. Every command in the script is
	// tolerated individually, so a non-zero status here means the shell
	// itself did not run -- and the partial output is still worth keeping,
	// because "kubectl: not found" is itself an answer.
	if err != nil {
		out += fmt.Sprintf("\n%scollection failed\n%v\n", sectionMarker, err)
	}
	return os.WriteFile(dest, redactBytes([]byte(out), secrets), 0o600)
}

// clusterStateScript is the reads to run on the node, in the order a person
// would run them.
//
// Reads only. This runs on a cluster a failed run is about to be judged on,
// and a collector that changed anything would be rewriting the evidence.
//
// `|| true` on every command rather than `set -e`: the interesting case is
// the one where some of these fail, and the first failure must not stop the
// rest. A cluster with no controller Deployment still has events worth reading.
//
// One line, separated by `;` rather than newlines, because the newlines do
// not reach the node: run 37561084841 collected `echo` and the first kubectl
// invocation concatenated into a single word, and nothing else, which is what
// a shell does when the separators arrive as backslash continuations and are
// deleted. A script with no newlines in it has nothing to lose that way. Run
// 37582097398 collected all nine sections with it in place.
//
// controller.Namespace() and not the nke-controller that nvkectl's bootstrap cluster
// uses: run 37582097398 found the controller running in controller.Namespace() on the site
// cluster and nothing at all in nke-controller. The two deploy paths do not
// agree on where the controller goes, and this one follows the site.
func clusterStateScript() string {
	kubectl := "kubectl --kubeconfig " + shellQuote(kubeconfigPath)
	inNamespace := kubectl + " -n " + shellQuote(controller.Namespace())
	selector := "app.kubernetes.io/name=" + controller.Service()

	sections := []struct{ title, cmd string }{
		// First, because it is what tells a not-ready workload apart from
		// an unreachable one.
		{"service and endpoints", inNamespace + " get service,endpoints,endpointslice -o wide"},
		{"pods", inNamespace + " get pods -o wide"},
		{"deployments", inNamespace + " get deployments,statefulsets -o wide"},
		{"pod detail", inNamespace + " describe pods"},
		// Sorted by time rather than the default, which groups by object and
		// hides the order things went wrong in.
		{"events", inNamespace + " get events --sort-by=.lastTimestamp"},
		{"controller logs", inNamespace + " logs -l " + shellQuote(selector) + " --all-containers --tail=200 --prefix"},
		// The restart before this one, which is the only copy of why a
		// crash-looping container died.
		{"controller logs, previous", inNamespace + " logs -l " + shellQuote(selector) + " --all-containers --tail=200 --prefix --previous"},
		{"nodes", kubectl + " get nodes -o wide"},
		{"workloads, all namespaces", kubectl + " get pods -A -o wide"},
	}

	stanzas := make([]string, 0, 2*len(sections))
	for _, s := range sections {
		stanzas = append(stanzas,
			"echo "+shellQuote(sectionMarker+s.title),
			s.cmd+" 2>&1 || true",
		)
	}
	return strings.Join(stanzas, "; ")
}
