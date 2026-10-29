// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spirehandover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Names the hardened SPIRE chart gives the server's objects.
const (
	serverName = "spire-server"
	serverPod  = serverName + "-0"
	agentName  = "spire-agent"
	dataPVC    = "spire-data-" + serverPod
	// serverSocket is the SPIRE server's default admin socket, which the chart mounts.
	serverSocket = "/tmp/spire-server/private/api.sock"
	// upstreamCAVolume is the pod volume holding the disk UpstreamAuthority's Secret.
	upstreamCAVolume      = "upstream-ca"
	tpmPubhashesConfigMap = "tpm-pubhashes"

	// markerAnnotation on the site StatefulSet records the fingerprint of the
	// seed bundle once the site server is verified to serve it.
	markerAnnotation = "nvke.nvidia.com/spire-handover-bundle"
	fieldManager     = "nvkectl-spire-handover"
)

var (
	clusterStaticEntryGVR = schema.GroupVersionResource{Group: "spire.spiffe.io", Version: "v1alpha1", Resource: "clusterstaticentries"}
	configMapGVR          = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

// serverConfig is the part of the chart's server.conf (JSON) the handover reads.
type serverConfig struct {
	Server struct {
		TrustDomain string `json:"trust_domain"`
	} `json:"server"`
	Plugins struct {
		NodeAttestor []struct {
			K8sPSAT *struct {
				PluginData struct {
					Clusters []map[string]json.RawMessage `json:"clusters"`
				} `json:"plugin_data"`
			} `json:"k8s_psat"`
		} `json:"NodeAttestor"`
		UpstreamAuthority []map[string]json.RawMessage `json:"UpstreamAuthority"`
		BundlePublisher   []struct {
			K8sConfigMap *struct {
				PluginData struct {
					Clusters []map[string]struct {
						Namespace string `json:"namespace"`
						Name      string `json:"configmap_name"`
						Key       string `json:"configmap_key"`
					} `json:"clusters"`
				} `json:"plugin_data"`
			} `json:"k8s_configmap"`
		} `json:"BundlePublisher"`
	} `json:"plugins"`
}

func (h *handover) statefulSet(ctx context.Context, c *cluster) (*appsv1.StatefulSet, error) {
	sts, err := c.kube.AppsV1().StatefulSets(h.cfg.Namespace).Get(ctx, serverName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the %s SPIRE server StatefulSet: %w", c.name, err)
	}
	return sts, nil
}

func (h *handover) serverConfig(ctx context.Context, c *cluster) (*serverConfig, error) {
	cm, err := c.kube.CoreV1().ConfigMaps(h.cfg.Namespace).Get(ctx, serverName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the %s SPIRE server ConfigMap: %w", c.name, err)
	}
	var conf serverConfig
	if err := json.Unmarshal([]byte(cm.Data["server.conf"]), &conf); err != nil {
		return nil, fmt.Errorf("parsing the %s SPIRE server.conf: %w", c.name, err)
	}
	return &conf, nil
}

// preflight checks, before anything is changed, that the site server can take
// over the seed's state, and returns the seed's X.509 authorities (DER).
func (h *handover) preflight(ctx context.Context) ([][]byte, error) {
	if _, err := h.site.kube.CoreV1().PersistentVolumeClaims(h.cfg.Namespace).Get(ctx, dataPVC, metav1.GetOptions{}); err != nil {
		return nil, fmt.Errorf("reading the site SPIRE data PVC: %w", err)
	}
	seedConf, err := h.serverConfig(ctx, h.seed)
	if err != nil {
		return nil, err
	}
	siteConf, err := h.serverConfig(ctx, h.site)
	if err != nil {
		return nil, err
	}
	if seedConf.Server.TrustDomain == "" || seedConf.Server.TrustDomain != siteConf.Server.TrustDomain {
		return nil, fmt.Errorf("trust domains differ: seed %q, site %q", seedConf.Server.TrustDomain, siteConf.Server.TrustDomain)
	}
	seedUA, err := h.upstreamAuthority(ctx, h.seed, seedConf)
	if err != nil {
		return nil, err
	}
	siteUA, err := h.upstreamAuthority(ctx, h.site, siteConf)
	if err != nil {
		return nil, err
	}
	// A server whose UpstreamAuthority differs from the one that signed its
	// stored CA discards that CA on start and mints a new one, which every node
	// would reject.
	if seedUA != siteUA {
		return nil, fmt.Errorf("UpstreamAuthority differs: seed %s, site %s", seedUA, siteUA)
	}
	h.seedAgentPrefixes = seedConf.psatAgentPrefixes(seedConf.Server.TrustDomain)
	h.siteConf = siteConf
	return h.publishedAuthorities(ctx, h.seed, seedConf)
}

// psatAgentPrefixes returns the SPIFFE ID prefix of agents the server attests
// with k8s_psat, one per configured Kubernetes cluster.
func (c *serverConfig) psatAgentPrefixes(trustDomain string) []string {
	var prefixes []string
	for _, attestor := range c.Plugins.NodeAttestor {
		if attestor.K8sPSAT == nil {
			continue
		}
		for _, clusters := range attestor.K8sPSAT.PluginData.Clusters {
			for name := range clusters {
				prefixes = append(prefixes, "spiffe://"+trustDomain+"/spire/agent/k8s_psat/"+name+"/")
			}
		}
	}
	return prefixes
}

// upstreamAuthority describes the server's UpstreamAuthority: its plugin
// config, plus the certificate for the disk plugin, or "none".
func (h *handover) upstreamAuthority(ctx context.Context, c *cluster, conf *serverConfig) (string, error) {
	if len(conf.Plugins.UpstreamAuthority) == 0 {
		return "none", nil
	}
	raw, err := json.Marshal(conf.Plugins.UpstreamAuthority)
	if err != nil {
		return "", fmt.Errorf("encoding the %s UpstreamAuthority: %w", c.name, err)
	}
	if _, disk := conf.Plugins.UpstreamAuthority[0]["disk"]; !disk {
		return string(raw), nil
	}
	sts, err := h.statefulSet(ctx, c)
	if err != nil {
		return "", err
	}
	secretName := ""
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == upstreamCAVolume && v.Secret != nil {
			secretName = v.Secret.SecretName
		}
	}
	if secretName == "" {
		return "", fmt.Errorf("the %s SPIRE server mounts no %s Secret", c.name, upstreamCAVolume)
	}
	secret, err := c.kube.CoreV1().Secrets(h.cfg.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the %s upstream CA Secret: %w", c.name, err)
	}
	sum := sha256.Sum256(bytes.TrimSpace(secret.Data["tls.crt"]))
	return string(raw) + " with certificate sha256:" + hex.EncodeToString(sum[:]), nil
}

// publishedAuthorities reads the X.509 authorities a server published to its
// bundle ConfigMap, which stays readable after the server stops.
func (h *handover) publishedAuthorities(ctx context.Context, c *cluster, conf *serverConfig) ([][]byte, error) {
	for _, publisher := range conf.Plugins.BundlePublisher {
		if publisher.K8sConfigMap == nil {
			continue
		}
		for _, clusters := range publisher.K8sConfigMap.PluginData.Clusters {
			for _, target := range clusters {
				cm, err := c.kube.CoreV1().ConfigMaps(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
				if err != nil {
					return nil, fmt.Errorf("reading the %s bundle ConfigMap %s/%s: %w", c.name, target.Namespace, target.Name, err)
				}
				data := []byte(cm.Data[target.Key])
				var authorities [][]byte
				if strings.HasSuffix(target.Key, ".spiffe") {
					authorities, err = jwksAuthorities(data)
				} else {
					authorities, err = pemAuthorities(data)
				}
				if err != nil {
					return nil, fmt.Errorf("parsing the %s bundle ConfigMap %s/%s: %w", c.name, target.Namespace, target.Name, err)
				}
				if len(authorities) == 0 {
					return nil, fmt.Errorf("the %s bundle ConfigMap %s/%s holds no X.509 authority", c.name, target.Namespace, target.Name)
				}
				return authorities, nil
			}
		}
	}
	return nil, fmt.Errorf("the %s SPIRE server publishes no bundle ConfigMap", c.name)
}

func pemAuthorities(data []byte) ([][]byte, error) {
	var out [][]byte
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return out, nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("parsing certificate: %w", err)
		}
		out = append(out, block.Bytes)
	}
}

func jwksAuthorities(data []byte) ([][]byte, error) {
	var bundle struct {
		Keys []struct {
			Use string   `json:"use"`
			X5C []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("decoding SPIFFE bundle: %w", err)
	}
	var out [][]byte
	for _, key := range bundle.Keys {
		if key.Use != "x509-svid" || len(key.X5C) == 0 {
			continue
		}
		der, err := base64.StdEncoding.DecodeString(key.X5C[0])
		if err != nil {
			return nil, fmt.Errorf("decoding x5c: %w", err)
		}
		out = append(out, der)
	}
	return out, nil
}

// fingerprint identifies a set of authorities independent of order.
func fingerprint(authorities [][]byte) string {
	sorted := slices.Clone(authorities)
	slices.SortFunc(sorted, bytes.Compare)
	sum := sha256.New()
	for _, der := range sorted {
		sum.Write(der)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// copyObjects applies the seed's ClusterStaticEntries and TPM allowlist on the site.
func (h *handover) copyObjects(ctx context.Context) error {
	entries, err := h.seed.dyn.Resource(clusterStaticEntryGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing seed ClusterStaticEntries: %w", err)
	}
	for i := range entries.Items {
		entry := &entries.Items[i]
		if _, err := h.site.dyn.Resource(clusterStaticEntryGVR).Apply(ctx, entry.GetName(), portable(entry, "spec"),
			metav1.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
			return fmt.Errorf("applying ClusterStaticEntry %s on the site: %w", entry.GetName(), err)
		}
	}
	h.log.Info("copied ClusterStaticEntries to the site", "count", len(entries.Items))

	cm, err := h.seed.dyn.Resource(configMapGVR).Namespace(h.cfg.Namespace).Get(ctx, tpmPubhashesConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		h.log.Info("the seed has no TPM allowlist ConfigMap", "configmap", tpmPubhashesConfigMap)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the seed %s ConfigMap: %w", tpmPubhashesConfigMap, err)
	}
	if _, err := h.site.dyn.Resource(configMapGVR).Namespace(h.cfg.Namespace).Apply(ctx, cm.GetName(), portable(cm, "data", "binaryData"),
		metav1.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
		return fmt.Errorf("applying the %s ConfigMap on the site: %w", tpmPubhashesConfigMap, err)
	}
	return nil
}

// portable returns src without the metadata its API server populated, keeping
// labels, annotations and the named top-level fields.
func portable(src *unstructured.Unstructured, fields ...string) *unstructured.Unstructured {
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(src.GetAPIVersion())
	out.SetKind(src.GetKind())
	out.SetName(src.GetName())
	out.SetNamespace(src.GetNamespace())
	out.SetLabels(src.GetLabels())
	out.SetAnnotations(src.GetAnnotations())
	for _, field := range fields {
		if v, ok := src.Object[field]; ok {
			out.Object[field] = runtime.DeepCopyJSONValue(v)
		}
	}
	return out
}

// stateMoved reports whether the site StatefulSet carries the marker for this seed bundle.
func (h *handover) stateMoved(ctx context.Context, marker string) (bool, error) {
	sts, err := h.statefulSet(ctx, h.site)
	if err != nil {
		return false, err
	}
	return sts.Annotations[markerAnnotation] == marker, nil
}

// startAndPrune starts the site server and evicts the seed cluster's own
// in-cluster agents, which came across with the seed's datastore.
func (h *handover) startAndPrune(ctx context.Context) error {
	if err := h.scale(ctx, h.site, 1); err != nil {
		return err
	}
	if err := h.waitServerReady(ctx); err != nil {
		return err
	}
	// Join tokens cannot be listed through the SPIRE server API, so unused
	// seed join tokens are left to expire.
	out, err := h.serverCLI(ctx, "agent", "list", "-output", "json")
	if err != nil {
		return fmt.Errorf("listing attested agents: %w", err)
	}
	var list struct {
		Agents []struct {
			ID struct {
				TrustDomain string `json:"trust_domain"`
				Path        string `json:"path"`
			} `json:"id"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return fmt.Errorf("decoding the attested agent list: %w", err)
	}
	for _, agent := range list.Agents {
		id := "spiffe://" + agent.ID.TrustDomain + agent.ID.Path
		if !slices.ContainsFunc(h.seedAgentPrefixes, func(prefix string) bool { return strings.HasPrefix(id, prefix) }) {
			continue
		}
		if _, err := h.serverCLI(ctx, "agent", "evict", "-spiffeID", id); err != nil {
			return fmt.Errorf("evicting agent %s: %w", id, err)
		}
		h.log.Info("evicted a seed agent from the site server", "agent", id)
	}
	return nil
}

// verify checks that the site server trusts every seed authority, then marks
// the state as moved.
func (h *handover) verify(ctx context.Context, seedAuthorities [][]byte, marker string) error {
	out, err := h.serverCLI(ctx, "bundle", "show", "-format", "pem")
	if err != nil {
		return fmt.Errorf("reading the site bundle: %w", err)
	}
	siteAuthorities, err := pemAuthorities([]byte(out))
	if err != nil {
		return fmt.Errorf("parsing the site bundle: %w", err)
	}
	for _, der := range seedAuthorities {
		// Containment, not equality: the site server may already have
		// prepared its next authority.
		if !slices.ContainsFunc(siteAuthorities, func(s []byte) bool { return bytes.Equal(s, der) }) {
			return errors.New("the site SPIRE server does not serve the seed's X.509 authorities")
		}
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{markerAnnotation: marker}}})
	if err != nil {
		return fmt.Errorf("encoding the marker patch: %w", err)
	}
	if _, err := h.site.kube.AppsV1().StatefulSets(h.cfg.Namespace).Patch(ctx, serverName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("marking the site SPIRE server: %w", err)
	}
	return nil
}

// restartSiteAgents restarts the site's in-cluster SPIRE agents once the site
// server publishes the seed's authorities. Without an upstream CA the agents
// still trust the site server's replaced root and would otherwise recover only
// after their rebootstrap delay. The marker on the pod template both triggers
// the rollout and records that it happened.
func (h *handover) restartSiteAgents(ctx context.Context, seedAuthorities [][]byte, marker string) error {
	ds, err := h.site.kube.AppsV1().DaemonSets(h.cfg.Namespace).Get(ctx, agentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the site SPIRE agent DaemonSet: %w", err)
	}
	if ds.Spec.Template.Annotations[markerAnnotation] != marker {
		if err := h.poll(ctx, "the site bundle ConfigMap to publish the seed's authorities", func(ctx context.Context) (bool, error) {
			published, err := h.publishedAuthorities(ctx, h.site, h.siteConf)
			if err != nil {
				return false, nil //nolint:nilerr // the ConfigMap is rewritten asynchronously after the server starts
			}
			for _, der := range seedAuthorities {
				if !slices.ContainsFunc(published, func(p []byte) bool { return bytes.Equal(p, der) }) {
					return false, nil
				}
			}
			return true, nil
		}); err != nil {
			return err
		}
		patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"annotations": map[string]string{markerAnnotation: marker}},
		}}})
		if err != nil {
			return fmt.Errorf("encoding the agent restart patch: %w", err)
		}
		if _, err := h.site.kube.AppsV1().DaemonSets(h.cfg.Namespace).Patch(ctx, agentName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("restarting the site SPIRE agents: %w", err)
		}
		h.log.Info("restarting the site SPIRE agents")
	}
	return h.poll(ctx, "the site SPIRE agents to roll out", func(ctx context.Context) (bool, error) {
		ds, err := h.site.kube.AppsV1().DaemonSets(h.cfg.Namespace).Get(ctx, agentName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("reading the site SPIRE agent DaemonSet: %w", err)
		}
		st := ds.Status
		return st.ObservedGeneration >= ds.Generation && st.UpdatedNumberScheduled == st.DesiredNumberScheduled &&
			st.NumberAvailable == st.DesiredNumberScheduled, nil
	})
}

// serverCLI runs the spire-server CLI in the site server pod.
func (h *handover) serverCLI(ctx context.Context, args ...string) (string, error) {
	// The CLI's subcommand comes first, then its flags.
	full := append([]string{"exec", serverPod, "-c", serverName, "--", "/opt/spire/bin/spire-server"}, args[:2]...)
	full = append(full, "-socketPath", serverSocket)
	full = append(full, args[2:]...)
	return h.kubectl(ctx, h.site, full...)
}
