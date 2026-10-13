// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The controller's names for a Machine's SPIRE agent identity. They must match
// constants.MachineAnnotationKeySpireAgentSpiffeIDPath, constants.TPMSpireAgentPath
// and constants.NamespaceNVKESystem in controller/internal/constants, which this
// module does not import.
const (
	spireAgentIDPathAnnotation = "nke.nvidia.com/spire-agent-spiffe-id-path"
	tpmSpireAgentPath          = "/spire/agent/tpm/"
	tenantMachineNamespace     = "nke-system"
)

// validateTPMAttestationPostCreate asserts that every node of the tenant
// joined with an identity its TPM proved.
//
// A Ready node does not show that on its own. The agent could have attested
// another way, and the controller writes a TPM path only once CAPNICo reports
// the machine's EK hash, so the chain this checks is the one a site relies on:
// the hash reached the Machine, and the SPIRE server holds an agent that
// attested as it.
func validateTPMAttestationPostCreate(test *tenantTest) {
	It("attests every node's SPIRE agent by its TPM", func(ctx context.Context) {
		want := clusterNodeCount(test.spec)
		var ids []string

		By("Reading the SPIRE server's trust domain")
		trustDomain, err := test.tenantClients.ManagementTrustDomain(ctx)
		Expect(err).NotTo(HaveOccurred(), "the agent IDs are built in the SPIRE server's trust domain")

		By("Reading each Machine's SPIRE agent identity")
		Eventually(func(ctx context.Context) error {
			machines := &unstructured.UnstructuredList{}
			machines.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"})
			if err := test.k8sClient.List(ctx, machines, client.InNamespace(tenantMachineNamespace),
				client.MatchingLabels{"cluster.x-k8s.io/cluster-name": test.cluster.Name}); err != nil {
				return fmt.Errorf("listing CAPI Machines: %w", err)
			}
			var err error
			ids, err = tpmAgentIDs(trustDomain, machines.Items, want)
			return err
		}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
			"every Machine should carry a TPM agent identity")

		By("Asking the SPIRE server for each attested agent")
		for _, id := range ids {
			out, err := test.tenantClients.ShowSPIREAgent(ctx, id)
			Expect(err).NotTo(HaveOccurred(), "the SPIRE server should list %s as attested", id)
			test.log.Info("attested SPIRE agent", "spiffeID", id, "server", strings.TrimSpace(out))
		}
	}, Label("readiness", "tpm-attestation"), NodeTimeout(5*time.Minute))
}

// tpmAgentIDs returns the SPIFFE ID each Machine's agent attests as, sorted,
// and an error naming every Machine that has no TPM identity or shares one
// with another Machine. want is the node count the cluster was created with:
// fewer Machines would let a node that never got one go unchecked.
func tpmAgentIDs(trustDomain string, machines []unstructured.Unstructured, want int) ([]string, error) {
	if trustDomain == "" {
		return nil, errors.New("no SPIRE trust domain to build the agent IDs in")
	}
	if len(machines) != want {
		return nil, fmt.Errorf("%d CAPI Machines for the cluster; want one per node, %d", len(machines), want)
	}
	var (
		ids      []string
		problems []string
		owners   = map[string]string{}
	)
	for _, machine := range machines {
		path := machine.GetAnnotations()[spireAgentIDPathAnnotation]
		if !strings.HasPrefix(path, tpmSpireAgentPath) || path == tpmSpireAgentPath {
			problems = append(problems, fmt.Sprintf("Machine %s has agent path %q, not a TPM identity", machine.GetName(), path))
			continue
		}
		if owner, seen := owners[path]; seen {
			problems = append(problems, fmt.Sprintf("Machine %s has the same TPM identity as %s", machine.GetName(), owner))
			continue
		}
		owners[path] = machine.GetName()
		ids = append(ids, "spiffe://"+trustDomain+path)
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return nil, errors.New(strings.Join(problems, "; "))
	}
	slices.Sort(ids)
	return ids, nil
}
