//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	coreDNSDeploymentPath   = "/apis/apps/v1/namespaces/kube-system/deployments/coredns"
	coreDNSDaemonSetPath    = "/apis/apps/v1/namespaces/kube-system/daemonsets/coredns"
	coreDNSPodPath          = "/api/v1/namespaces/kube-system/pods/coredns-0"
	coreDNSPodsPath         = "/api/v1/namespaces/kube-system/pods"
	kubeDNSServicePath      = "/api/v1/namespaces/kube-system/services/kube-dns"
	kubeSystemNamespacePath = "/api/v1/namespaces/kube-system"
	nodeLocalDNSPath        = "/apis/apps/v1/namespaces/kube-system/daemonsets/node-local-dns"
	secretCollectionPath    = "/api/v1/namespaces/nke-system/secrets"

	// fixtureResourceVersion is shared by every stored object. The fixture never
	// replays incremental watch events, so a single version is enough for
	// client-go to accept the stream.
	fixtureResourceVersion = "1"
)

// TenantAPIServer is the narrow Kubernetes API fixture needed by the production
// InstallKubeLocalDNS and LabelSystemNamespacesForPSA activities in the gofail
// crash-recovery scenario.
type TenantAPIServer struct {
	server *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
}

// StartTenantAPIServer starts a tenant API fixture with kubeadm-managed DNS resources.
func StartTenantAPIServer(t testing.TB) *TenantAPIServer {
	t.Helper()

	fixture := &TenantAPIServer{
		objects: map[string][]byte{},
	}
	fixture.store(coreDNSDeploymentPath, newGofailCoreDNSDeployment())
	fixture.store(coreDNSPodPath, newGofailCoreDNSPod())
	fixture.store(kubeDNSServicePath, newGofailKubeDNSService())
	fixture.store(kubeSystemNamespacePath, newGofailKubeSystemNamespace())
	fixture.server = httptest.NewServer(fixture)
	t.Cleanup(fixture.server.Close)
	return fixture
}

// Kubeconfig returns client configuration for the tenant API fixture.
func (s *TenantAPIServer) Kubeconfig() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: gofail-tenant
  cluster:
    server: %s
contexts:
- name: gofail-tenant
  context:
    cluster: gofail-tenant
    user: gofail-tenant
current-context: gofail-tenant
users:
- name: gofail-tenant
  user: {}
`, s.server.URL)
}

// WriteKubeconfig writes client configuration for CWE's management-client bootstrap.
func (s *TenantAPIServer) WriteKubeconfig(t testing.TB) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(s.Kubeconfig()), 0o600))
	return path
}

// AssertDNSReconciled verifies that the production activity reached its final
// label and Service-selector cutover, rather than merely accepting API calls.
func (s *TenantAPIServer) AssertDNSReconciled(t testing.TB) {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	var pod corev1.Pod
	require.NoError(t, json.Unmarshal(s.objects[coreDNSPodPath], &pod))
	require.Equal(t, "cluster-dns", pod.Labels["dns-role"])

	var service corev1.Service
	require.NoError(t, json.Unmarshal(s.objects[kubeDNSServicePath], &service))
	require.Equal(t, map[string]string{"dns-role": "cluster-dns"}, service.Spec.Selector)
	require.Equal(t, "node-local-dns", service.Annotations["k8s.ovn.org/prefer-local-with-fallback"])
	require.Contains(t, s.objects, nodeLocalDNSPath)
}

func (s *TenantAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet &&
		r.URL.Path == secretCollectionPath &&
		r.URL.Query().Get("watch") == "true" {
		s.streamSecretWatch(w, r)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/version":
		writeJSON(w, http.StatusOK, map[string]string{
			"major":      "1",
			"minor":      "31",
			"gitVersion": "v1.31.0",
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api":
		writeJSON(w, http.StatusOK, &metav1.APIVersions{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIVersions"},
			Versions: []string{"v1"},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1":
		writeJSON(w, http.StatusOK, &metav1.APIResourceList{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				namespacedAPIResource("configmaps", "configmap", "ConfigMap"),
				clusterScopedAPIResource("namespaces", "namespace", "Namespace"),
				namespacedAPIResource("pods", "pod", "Pod"),
				namespacedAPIResource("secrets", "secret", "Secret"),
				namespacedAPIResource("serviceaccounts", "serviceaccount", "ServiceAccount"),
				namespacedAPIResource("services", "service", "Service"),
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/apis":
		writeJSON(w, http.StatusOK, &metav1.APIGroupList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIGroupList"},
			Groups: []metav1.APIGroup{{
				Name: "apps",
				Versions: []metav1.GroupVersionForDiscovery{{
					GroupVersion: "apps/v1",
					Version:      "v1",
				}},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "apps/v1",
					Version:      "v1",
				},
			}},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1":
		writeJSON(w, http.StatusOK, &metav1.APIResourceList{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				namespacedAPIResource("daemonsets", "daemonset", "DaemonSet"),
				namespacedAPIResource("deployments", "deployment", "Deployment"),
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == secretCollectionPath:
		s.writeSecretList(w)
	case r.Method == http.MethodGet && r.URL.Path == coreDNSDaemonSetPath:
		writeNotFound(w, "apps", "daemonsets", "coredns")
	case r.Method == http.MethodGet && r.URL.Path == coreDNSPodsPath:
		s.writeCoreDNSPodList(w)
	case r.Method == http.MethodGet:
		s.writeObject(w, r.URL.Path)
	case r.Method == http.MethodPatch:
		s.patchObject(w, r)
	case r.Method == http.MethodPost && r.URL.Path == secretCollectionPath:
		s.createSecret(w, r)
	case r.Method == http.MethodPut:
		s.writeObject(w, r.URL.Path)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
			fmt.Sprintf("unsupported gofail tenant API request: %s %s", r.Method, r.URL.RequestURI()))
	}
}

func namespacedAPIResource(name, singularName, kind string) metav1.APIResource {
	return metav1.APIResource{
		Name:         name,
		SingularName: singularName,
		Namespaced:   true,
		Kind:         kind,
		Verbs:        metav1.Verbs{"create", "get", "list", "patch", "update", "watch"},
	}
}

func clusterScopedAPIResource(name, singularName, kind string) metav1.APIResource {
	return metav1.APIResource{
		Name:         name,
		SingularName: singularName,
		Namespaced:   false,
		Kind:         kind,
		Verbs:        metav1.Verbs{"create", "get", "list", "patch", "update", "watch"},
	}
}

func (s *TenantAPIServer) writeSecretList(w http.ResponseWriter) {
	secrets, err := s.listSecrets()
	if err != nil {
		writeStatus(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, &corev1.SecretList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"},
		ListMeta: metav1.ListMeta{ResourceVersion: fixtureResourceVersion},
		Items:    secrets,
	})
}

// streamSecretWatch serves the informer's watch. client-go requests its initial
// list through this endpoint with sendInitialEvents, and only treats the cache
// as synced once a bookmark carrying the initial-events-end annotation arrives,
// so the stream must emit that bookmark before it goes idle.
func (s *TenantAPIServer) streamSecretWatch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	secrets, err := s.listSecrets()
	s.mu.Unlock()
	if err != nil {
		writeStatus(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	encoder := json.NewEncoder(w)
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		for i := range secrets {
			if err := encodeWatchEvent(encoder, "ADDED", &secrets[i]); err != nil {
				return
			}
		}
		if err := encodeWatchEvent(encoder, "BOOKMARK", initialEventsEndBookmark()); err != nil {
			return
		}
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	<-r.Context().Done()
}

func (s *TenantAPIServer) listSecrets() ([]corev1.Secret, error) {
	secrets := make([]corev1.Secret, 0)
	for path, data := range s.objects {
		if !strings.HasPrefix(path, secretCollectionPath+"/") {
			continue
		}
		var secret corev1.Secret
		if err := json.Unmarshal(data, &secret); err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

func encodeWatchEvent(encoder *json.Encoder, eventType string, secret *corev1.Secret) error {
	object, err := json.Marshal(secret)
	if err != nil {
		return err
	}
	return encoder.Encode(struct {
		Type   string          `json:"type"`
		Object json.RawMessage `json:"object"`
	}{Type: eventType, Object: object})
}

func initialEventsEndBookmark() *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			ResourceVersion: fixtureResourceVersion,
			Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
		},
	}
}

func (s *TenantAPIServer) createSecret(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	var secret corev1.Secret
	if err := json.Unmarshal(data, &secret); err != nil {
		// Typed client-go writes use Kubernetes protobuf by default. The fixture
		// only needs the object identity so later Get/Update calls can succeed.
		secret = corev1.Secret{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cwe-gofail-cluster-kubeconfig",
				Namespace: "nke-system",
			},
		}
	}
	if secret.APIVersion == "" {
		secret.APIVersion = "v1"
	}
	if secret.Kind == "" {
		secret.Kind = "Secret"
	}
	secret.ResourceVersion = fixtureResourceVersion
	data, err = json.Marshal(&secret)
	if err != nil {
		writeStatus(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	s.objects[secretCollectionPath+"/"+secret.Name] = data
	writeRawJSON(w, http.StatusCreated, data)
}

func (s *TenantAPIServer) store(path string, obj any) {
	data, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("marshal gofail tenant API fixture: %v", err))
	}
	s.objects[path] = data
}

func (s *TenantAPIServer) writeObject(w http.ResponseWriter, path string) {
	data, ok := s.objects[path]
	if !ok {
		writeNotFound(w, "", "resource", path[strings.LastIndex(path, "/")+1:])
		return
	}
	writeRawJSON(w, http.StatusOK, data)
}

func (s *TenantAPIServer) writeCoreDNSPodList(w http.ResponseWriter) {
	podData := s.objects[coreDNSPodPath]
	var pod corev1.Pod
	if err := json.Unmarshal(podData, &pod); err != nil {
		writeStatus(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, &corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items:    []corev1.Pod{pod},
	})
}

func (s *TenantAPIServer) patchObject(w http.ResponseWriter, r *http.Request) {
	patch, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}

	if strings.Contains(r.Header.Get("Content-Type"), "apply-patch") {
		patch, err = utilyaml.ToJSON(patch)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		s.objects[r.URL.Path] = patch
		writeRawJSON(w, http.StatusOK, patch)
		return
	}

	current, ok := s.objects[r.URL.Path]
	if !ok {
		writeNotFound(w, "", "resource", r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		return
	}
	updated, err := jsonpatch.MergePatch(current, patch)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	s.objects[r.URL.Path] = updated
	writeRawJSON(w, http.StatusOK, updated)
}

func writeNotFound(w http.ResponseWriter, group, resource, name string) {
	message := fmt.Sprintf("%s %q not found", resource, name)
	writeJSON(w, http.StatusNotFound, &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   metav1.StatusReasonNotFound,
		Details: &metav1.StatusDetails{
			Name:  name,
			Group: group,
			Kind:  resource,
		},
		Code: http.StatusNotFound,
	})
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	writeJSON(w, code, &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   reason,
		Code:     int32(code),
	})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeRawJSON(w, code, data)
}

func writeRawJSON(w http.ResponseWriter, code int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func newGofailCoreDNSDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns",
			Namespace: metav1.NamespaceSystem,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"k8s-app": "kube-dns"}},
			},
		},
	}
}

func newGofailCoreDNSPod() *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns-0",
			Namespace: metav1.NamespaceSystem,
			Labels:    map[string]string{"k8s-app": "kube-dns"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func newGofailKubeSystemNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespaceSystem},
	}
}

func newGofailKubeDNSService() *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-dns",
			Namespace: metav1.NamespaceSystem,
			Annotations: map[string]string{
				"prometheus.io/port":   "9153",
				"prometheus.io/scrape": "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"k8s-app": "kube-dns"},
			Ports: []corev1.ServicePort{
				{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt(53)},
				{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(53)},
				{Name: "metrics", Port: 9153, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(9153)},
			},
		},
	}
}
