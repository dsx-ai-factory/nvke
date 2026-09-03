// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package providers is the registry of Cluster API infrastructure providers
// nkectl knows how to describe.
//
// It holds data only — API kinds, CRD names, how a provider's manager is
// identified, and where its credentials Secret lives. Nothing here talks to a
// cluster, so a request builder can depend on the registry without depending
// on anything that shells out to kubectl. See providers/inspect for the half
// that does.
//
// The values here are read from each provider's own source. They are a
// snapshot: both providers are under active development, so a bump should
// re-check the CRD names, the manager identity, and the credentials keys.
package providers

// Type identifies an infrastructure provider in the set nkectl can describe.
//
// The set is closed so that nkectl only claims to understand providers it has
// been taught about. It is not enforcement: CWE accepts any capiInfraCluster
// manifest, so a provider absent from this list can still be used by writing
// that manifest by hand.
type Type string

const (
	// TypeNico is cluster-api-provider-nico, backed by the NICo bare-metal API.
	TypeNico Type = "nico"

	// TypeLaunchLayer is cluster-api-provider-launchlayer, the quota and
	// placement layer that sits above NICo.
	TypeLaunchLayer Type = "launchlayer"
)

// cluster-api-provider-provisioned is deliberately absent. It adopts hosts NKE
// provisioned for a site controller during `nkectl bootstrap site`, takes no
// credentials, and is never a backend an operator chooses for a tenant cluster.
// Listing it here would answer a question nobody asks and imply it is a
// selectable alternative to NICo or LaunchLayer.

// Descriptor is everything nkectl knows about one infrastructure provider.
type Descriptor struct {
	// Type names the provider.
	Type Type `json:"type"`

	// CWEProviderType is the value CWE's own provider_type discriminator takes
	// for clusters backed by this provider. It is recorded so the CLI's
	// vocabulary and CWE's cannot silently diverge: CWE distinguishes only
	// "nico" from "capi", while this registry names each provider.
	CWEProviderType string `json:"cweProviderType"`

	// ClusterKind and MachineTemplateKind are the infrastructure kinds a
	// cluster request references.
	ClusterKind         string `json:"clusterKind"`
	MachineTemplateKind string `json:"machineTemplateKind"`

	// CRDNames are the CRDs that must be established, and serving at least one
	// version, for the provider to count as installed. Only the CRDs nkectl
	// depends on are listed, not every CRD the provider ships.
	CRDNames []string `json:"crdNames"`

	// AppName is the app.kubernetes.io/name value the provider's manager
	// carries. This is the primary way the manager Deployment is found.
	//
	// The CAPI-conventional cluster.x-k8s.io/provider label is deliberately not
	// used: cluster-api-provider-nico does not set it anywhere in its config or
	// chart, so keying on it would report NICo's controller as missing.
	AppName string `json:"appName"`

	// ControllerNamespace and ControllerDeployment are the manager's default
	// identity, used as a fallback when AppName finds nothing. They vary with
	// how the provider was installed — a kustomize namePrefix or a Helm release
	// name changes the Deployment name — so they are a hint, not a guarantee.
	ControllerNamespace  string `json:"controllerNamespace"`
	ControllerDeployment string `json:"controllerDeployment"`

	// SecretNameFlag and SecretNamespaceFlag are the manager flags that name
	// the provider-level credentials Secret. They differ per provider, so
	// reading them off a running manager's args needs the right flag name.
	SecretNameFlag      string `json:"secretNameFlag,omitempty"`
	SecretNamespaceFlag string `json:"secretNamespaceFlag,omitempty"`

	// DefaultSecretName is the credentials Secret name assumed when the manager
	// is not running and so cannot be asked. Empty when the provider has no
	// default of its own.
	DefaultSecretName string `json:"defaultSecretName,omitempty"`

	// EndpointFlag is the manager flag carrying the address of the backend the
	// provider calls, for providers configured that way.
	//
	// The two providers differ here. NICo takes its endpoint as a key in the
	// credentials Secret, so it arrives with the credentials and this is empty.
	// LaunchLayer takes one address per deployment as a flag, and an absent
	// address is not a startup failure — the manager runs and reports it once
	// per reconcile. That combination is reportable: established CRDs, a
	// running manager and a valid Secret, yet nothing the provider can call.
	EndpointFlag string `json:"endpointFlag,omitempty"`

	// UnconfiguredIsValid records that the provider runs correctly with no
	// credentials Secret at all. LaunchLayer does: with no Secret its client is
	// unauthenticated, which is how it addresses a fake. For such a provider a
	// missing Secret is a configuration choice, not a fault.
	UnconfiguredIsValid bool `json:"unconfiguredIsValid"`
}

// infrastructureGroup is the API group every CAPI infrastructure provider
// serves its cluster and machine-template kinds from.
const infrastructureGroup = "infrastructure.cluster.x-k8s.io"

// All returns every provider in the registry, in a stable order.
func All() []Descriptor {
	return []Descriptor{
		{
			Type:                 TypeNico,
			CWEProviderType:      "capi",
			ClusterKind:          "NicoCluster",
			MachineTemplateKind:  "NicoMachineTemplate",
			CRDNames:             []string{"nicoclusters." + infrastructureGroup, "nicomachinetemplates." + infrastructureGroup},
			AppName:              "cluster-api-provider-nico",
			ControllerNamespace:  "capnico-system",
			ControllerDeployment: "controller-manager",
			SecretNameFlag:       "--provider-credentials-secret-name",
			SecretNamespaceFlag:  "--provider-credentials-namespace",
			DefaultSecretName:    "nico-credentials",
		},
		{
			Type:                 TypeLaunchLayer,
			CWEProviderType:      "capi",
			ClusterKind:          "LaunchLayerCluster",
			MachineTemplateKind:  "LaunchLayerMachineTemplate",
			CRDNames:             []string{"launchlayerclusters." + infrastructureGroup, "launchlayermachinetemplates." + infrastructureGroup},
			AppName:              "cluster-api-provider-launchlayer",
			ControllerNamespace:  "capll-system",
			ControllerDeployment: "cluster-api-provider-launchlayer-controller-manager",
			SecretNameFlag:       "--identity-secret",
			SecretNamespaceFlag:  "--identity-secret-namespace",
			EndpointFlag:         "--base-url",
			// CAPLL has no default: --identity-secret names the Secret, and
			// omitting it leaves the client unauthenticated on purpose.
			DefaultSecretName:   "",
			UnconfiguredIsValid: true,
		},
	}
}

// Lookup returns the descriptor for t, and whether t is in the registry.
func Lookup(t Type) (Descriptor, bool) {
	for _, d := range All() {
		if d.Type == t {
			return d, true
		}
	}
	return Descriptor{}, false
}

// Types returns the registry's provider names, for use in flag help and in the
// error raised for an unsupported --type.
func Types() []string {
	all := All()
	names := make([]string, 0, len(all))
	for _, d := range all {
		names = append(names, string(d.Type))
	}
	return names
}
