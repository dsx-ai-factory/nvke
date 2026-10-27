// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spirehandover

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

const (
	trustDomain = "site.example"
	siteAgent   = "spiffe://site.example/spire/agent/tpm/node-a"
	seedAgent   = "spiffe://site.example/spire/agent/k8s_psat/seed/kind-node"
	// siteKubeAgent is one of the site's own in-cluster agents; prune must keep it.
	siteKubeAgent = "spiffe://site.example/spire/agent/k8s_psat/site/node-a"
	siteAddress   = "192.0.2.10"
	sitePort      = 30440
)

var clusterID = openapi_types.UUID{1}

// recorder is the ordered log of mutating calls across both clusters and kubectl.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) index(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.IndexFunc(r.events, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

func (r *recorder) count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// fakeRunner answers the kubectl calls the handover makes.
type fakeRunner struct {
	rec          *recorder
	bundlePEM    string
	siteChecksum string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cluster := "seed"
	if args[1] == "site.kubeconfig" {
		cluster = "site"
	}
	cmd := strings.Join(args[4:], " ")
	f.rec.add(cluster + " " + cmd)
	switch {
	case name != "kubectl":
		return "", fmt.Errorf("unexpected command %s", name)
	case strings.Contains(cmd, "ls -1"):
		return "datastore.sqlite3\ndatastore.sqlite3-wal\nkeys.json\ntpm-direct\n", nil
	case strings.Contains(cmd, "sha256sum"):
		sum := "aaaa"
		if cluster == "site" && f.siteChecksum != "" {
			sum = f.siteChecksum
		}
		var out strings.Builder
		for _, p := range args[slices.Index(args, "sha256sum")+1:] {
			fmt.Fprintf(&out, "%s  %s\n", sum, p)
		}
		return out.String(), nil
	case strings.Contains(cmd, "agent list"):
		return `{"agents":[{"id":{"trust_domain":"site.example","path":"/spire/agent/tpm/node-a"}},` +
			`{"id":{"trust_domain":"site.example","path":"/spire/agent/k8s_psat/seed/kind-node"}},` +
			`{"id":{"trust_domain":"site.example","path":"/spire/agent/k8s_psat/site/node-a"}}]}`, nil
	case strings.Contains(cmd, "bundle show"):
		return f.bundlePEM, nil
	}
	return "", nil
}

type fixture struct {
	t                      *testing.T
	seedTD, siteTD         string
	seedCACert, siteCACert string
	marked                 bool
	siteChecksum           string
	agentsRestarted        bool // the site agent DaemonSet already carries the marker
	siteBundleStale        bool // the site bundle ConfigMap does not hold the seed's authority yet
}

type built struct {
	h          *handover
	rec        *recorder
	seedKube   *fake.Clientset
	siteKube   *fake.Clientset
	siteDyn    *dynamicfake.FakeDynamicClient
	applied    []*unstructured.Unstructured
	authority  []byte
	controller *fakeController
}

func testCert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "spire"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return der
}

func serverConf(td string, upstream bool) string {
	conf := map[string]any{
		"server": map[string]any{"trust_domain": td},
		"plugins": map[string]any{
			"NodeAttestor": []any{
				map[string]any{"k8s_psat": map[string]any{"plugin_data": map[string]any{
					"clusters": []any{map[string]any{"seed": map[string]any{"audience": []any{"spire-server"}}}},
				}}},
				map[string]any{"tpm": map[string]any{"plugin_data": map[string]any{}}},
			},
			"BundlePublisher": []any{map[string]any{"k8s_configmap": map[string]any{"plugin_data": map[string]any{
				"clusters": []any{map[string]any{"chart-internal": map[string]any{
					"namespace": "spire-server", "configmap_name": "spire-bundle", "configmap_key": "bundle.spiffe", "format": "spiffe",
				}}},
			}}}},
		},
	}
	if upstream {
		conf["plugins"].(map[string]any)["UpstreamAuthority"] = []any{map[string]any{"disk": map[string]any{
			"plugin_data": map[string]any{"cert_file_path": "/run/spire/upstream_ca/tls.crt"},
		}}}
	}
	out, _ := json.Marshal(conf)
	return string(out)
}

func serverObjects(td, caCert, secretName string, annotations map[string]string) []runtime.Object {
	objs := []runtime.Object{
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: DefaultNamespace, Annotations: annotations},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "spire-server"}},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "chown", Image: "mirror.example/busybox:1.37.0-uclibc"}},
					Volumes: []corev1.Volume{{Name: upstreamCAVolume, VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: secretName},
					}}},
				}},
			},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: DefaultNamespace},
			Data:       map[string]string{"server.conf": serverConf(td, caCert != "")},
		},
	}
	if caCert != "" {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: DefaultNamespace},
			Data:       map[string][]byte{"tls.crt": []byte(caCert)},
		})
	}
	return objs
}

func (f fixture) build() *built {
	t := f.t
	if f.seedTD == "" {
		f.seedTD = trustDomain
	}
	if f.siteTD == "" {
		f.siteTD = trustDomain
	}
	b := &built{rec: &recorder{}, authority: testCert(t)}
	bundle, err := json.Marshal(map[string]any{"keys": []any{
		map[string]any{"use": "x509-svid", "kty": "EC", "x5c": []string{base64.StdEncoding.EncodeToString(b.authority)}},
		map[string]any{"use": "jwt-svid", "kty": "EC"},
	}})
	require.NoError(t, err)

	seedObjs := append(serverObjects(f.seedTD, f.seedCACert, "spire-server-upstream-ca", nil),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "spire-bundle", Namespace: DefaultNamespace},
			Data:       map[string]string{"bundle.spiffe": string(bundle)},
		})
	var annotations map[string]string
	if f.marked {
		annotations = map[string]string{markerAnnotation: fingerprint([][]byte{b.authority})}
	}
	siteBundle := bundle
	if f.siteBundleStale {
		siteBundle, err = json.Marshal(map[string]any{"keys": []any{
			map[string]any{"use": "x509-svid", "kty": "EC", "x5c": []string{base64.StdEncoding.EncodeToString(testCert(t))}},
		}})
		require.NoError(t, err)
	}
	var templateAnnotations map[string]string
	if f.agentsRestarted {
		templateAnnotations = map[string]string{markerAnnotation: fingerprint([][]byte{b.authority})}
	}
	siteObjs := append(serverObjects(f.siteTD, f.siteCACert, "spire-ca", annotations),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dataPVC, Namespace: DefaultNamespace}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "spire-bundle", Namespace: DefaultNamespace},
			Data:       map[string]string{"bundle.spiffe": string(siteBundle)},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: DefaultNamespace},
			Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: templateAnnotations}}},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3},
		})

	b.seedKube = fake.NewClientset(seedObjs...)
	b.siteKube = fake.NewClientset(siteObjs...)
	for name, kube := range map[string]*fake.Clientset{"seed": b.seedKube, "site": b.siteKube} {
		kube.PrependReactor("patch", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			b.rec.add(name + " patch statefulset " + string(action.(k8stesting.PatchAction).GetPatch()))
			return false, nil, nil
		})
		kube.PrependReactor("patch", "daemonsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			b.rec.add(name + " patch daemonset " + string(action.(k8stesting.PatchAction).GetPatch()))
			return false, nil, nil
		})
		kube.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			b.rec.add(name + " create pod " + pod.Spec.Containers[0].Image)
			pod.Status.Phase = corev1.PodRunning
			return false, nil, nil
		})
	}

	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{clusterStaticEntryGVR: "ClusterStaticEntryList"}
	entry := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "spire.spiffe.io/v1alpha1", "kind": "ClusterStaticEntry",
		"metadata": map[string]any{
			"name": "node-a", "uid": "seed-uid", "resourceVersion": "42", "creationTimestamp": "2026-01-01T00:00:00Z",
			"managedFields":   []any{map[string]any{"manager": "nvke-controller"}},
			"ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "seed-owner", "uid": "x"}},
			"labels":          map[string]any{"app": "nvke"},
			"annotations":     map[string]any{"note": "kept"},
		},
		"spec":   map[string]any{"parentID": siteAgent, "spiffeID": "spiffe://site.example/node/a"},
		"status": map[string]any{"rendered": true},
	}}
	pubhashes := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": tpmPubhashesConfigMap, "namespace": DefaultNamespace, "uid": "u", "resourceVersion": "7"},
		"data":     map[string]any{"abc": ""},
	}}
	seedDyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, entry, pubhashes)
	b.siteDyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	b.siteDyn.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := &unstructured.Unstructured{}
		require.NoError(t, json.Unmarshal(action.(k8stesting.PatchAction).GetPatch(), &obj.Object))
		b.applied = append(b.applied, obj)
		return true, obj, nil
	})

	b.controller = &fakeController{status: [][]api.NodeSpireAgentStatus{{onSite("node-a")}}}
	srv := httptest.NewServer(b.controller)
	t.Cleanup(srv.Close)
	client, err := clients.New(config.SiteConfig{URL: srv.URL})
	require.NoError(t, err)

	runner := &fakeRunner{
		rec: b.rec, siteChecksum: f.siteChecksum,
		bundlePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.authority})),
	}
	b.h = newHandover(Config{
		Controller: client, ClusterID: clusterID, SiteSpireAddress: siteAddress, SiteSpirePort: sitePort,
		PollInterval: time.Millisecond, PollTimeout: 200 * time.Millisecond, Runner: runner,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	},
		&cluster{name: "seed", kubeconfig: "seed.kubeconfig", kube: b.seedKube, dyn: seedDyn},
		&cluster{name: "site", kubeconfig: "site.kubeconfig", kube: b.siteKube, dyn: b.siteDyn})
	return b
}

// fakeController serves the seed controller's SPIRE server endpoints.
type fakeController struct {
	mu         sync.Mutex
	patchCode  int
	inProgress api.UpdateInProgressType
	requested  *api.SpireServer
	status     [][]api.NodeSpireAgentStatus // successive GET answers; the last repeats
	patches    int
	failed     string
}

func (c *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/spire-server"):
		c.patches++
		code := c.patchCode
		if code == 0 {
			code = http.StatusAccepted
		}
		w.WriteHeader(code)
		if code == http.StatusConflict {
			_, _ = w.Write([]byte(`{"message":"another operation is in progress"}`))
		}
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/spire-server"):
		nodes := c.status[0]
		if len(c.status) > 1 {
			c.status = c.status[1:]
		}
		_ = json.NewEncoder(w).Encode(api.ClusterSpireServerStatus{Requested: c.requested, Nodes: nodes})
	case r.Method == http.MethodGet:
		cluster := api.Cluster{}
		if c.inProgress != "" {
			cluster.Conditions.UpdateInProgress = &api.UpdateInProgress{Type: c.inProgress}
		}
		if c.failed != "" {
			cluster.Conditions.UpdateFailed = &api.Condition{Status: api.ConditionStatusTrue, Message: c.failed}
		}
		_ = json.NewEncoder(w).Encode(cluster)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func onSite(name string) api.NodeSpireAgentStatus {
	addr, port := siteAddress, sitePort
	return api.NodeSpireAgentStatus{Name: name, Reachable: true, HasValidSVID: true, ServerAddress: &addr, ServerPort: &port}
}

func TestPreflightRefusesMismatchedServers(t *testing.T) {
	tests := []struct {
		name    string
		fixture fixture
		wantErr string
	}{
		{name: "trust domains differ", fixture: fixture{siteTD: "other.example"}, wantErr: "trust domains differ"},
		{name: "only the seed has an UpstreamAuthority", fixture: fixture{seedCACert: "CERT-A"}, wantErr: "UpstreamAuthority differs"},
		{name: "upstream certificates differ", fixture: fixture{seedCACert: "CERT-A", siteCACert: "CERT-B"}, wantErr: "UpstreamAuthority differs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fixture.t = t
			b := tt.fixture.build()
			err := b.h.run(context.Background())
			require.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, b.rec.events, "nothing may change before preflight passes")
			assert.Empty(t, b.applied)
		})
	}
}

func TestPreflightAcceptsTheSameUpstreamCA(t *testing.T) {
	b := fixture{t: t, seedCACert: "CERT-A\n", siteCACert: "CERT-A"}.build()
	authorities, err := b.h.preflight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, [][]byte{b.authority}, authorities)
}

func TestCopyObjectsStripsServerMetadata(t *testing.T) {
	b := fixture{t: t}.build()
	require.NoError(t, b.h.copyObjects(context.Background()))

	require.Len(t, b.applied, 2)
	entry := b.applied[0]
	assert.Equal(t, map[string]any{
		"name":        "node-a",
		"labels":      map[string]any{"app": "nvke"},
		"annotations": map[string]any{"note": "kept"},
	}, entry.Object["metadata"])
	assert.Equal(t, map[string]any{"parentID": siteAgent, "spiffeID": "spiffe://site.example/node/a"}, entry.Object["spec"])
	assert.NotContains(t, entry.Object, "status")

	cm := b.applied[1]
	assert.Equal(t, tpmPubhashesConfigMap, cm.GetName())
	assert.Equal(t, DefaultNamespace, cm.GetNamespace())
	assert.Empty(t, cm.GetUID())
	assert.Empty(t, cm.GetResourceVersion())
	assert.Equal(t, map[string]any{"abc": ""}, cm.Object["data"])
}

func TestRunMovesStateAfterStoppingBothServers(t *testing.T) {
	b := fixture{t: t}.build()
	require.NoError(t, b.h.run(context.Background()))

	siteStop := b.rec.index(`site patch statefulset {"spec":{"replicas":0}}`)
	seedStop := b.rec.index(`seed patch statefulset {"spec":{"replicas":0}}`)
	firstCopy := b.rec.index("seed cp")
	require.NotEqual(t, -1, siteStop)
	require.NotEqual(t, -1, seedStop)
	require.NotEqual(t, -1, firstCopy)
	assert.Less(t, siteStop, firstCopy)
	assert.Less(t, seedStop, firstCopy)
	assert.Less(t, b.rec.index("site exec "+helperPod+" -c helper -- rm -f"), b.rec.index("site cp"))
	assert.Equal(t, 1, b.rec.count("seed create pod mirror.example/busybox:1.37.0-uclibc"))
	// datastore, keys and the WAL present on the seed, not the absent -shm.
	assert.Equal(t, 3, b.rec.count("site cp"))
	assert.NotEqual(t, -1, b.rec.index("site exec "+helperPod+" -c helper -- chown 1000:1000"))
	assert.Less(t, b.rec.index(`site patch statefulset {"spec":{"replicas":1}}`), b.rec.index("site exec spire-server-0"))

	sts, err := b.siteKube.AppsV1().StatefulSets(DefaultNamespace).Get(context.Background(), serverName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, fingerprint([][]byte{b.authority}), sts.Annotations[markerAnnotation])
	pods, err := b.siteKube.CoreV1().Pods(DefaultNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pods.Items, "helper pods are deleted")
	assert.Equal(t, 1, b.controller.patches)
}

func TestRunSkipsTheMoveWhenTheSiteIsMarked(t *testing.T) {
	b := fixture{t: t, marked: true}.build()
	require.NoError(t, b.h.run(context.Background()))
	assert.Equal(t, -1, b.rec.index("seed patch statefulset"), "the seed server is not touched")
	assert.Equal(t, -1, b.rec.index("site cp"))
	assert.Equal(t, -1, b.rec.index("site create pod"))
	assert.Equal(t, 1, b.controller.patches)
}

func TestMoveStateFailsOnChecksumMismatch(t *testing.T) {
	b := fixture{t: t, siteChecksum: "bbbb"}.build()
	err := b.h.moveState(context.Background())
	require.ErrorContains(t, err, "checksum of /data/datastore.sqlite3 differs")
}

func TestStartAndPruneEvictsOnlyTheSeedClustersAgents(t *testing.T) {
	b := fixture{t: t}.build()
	_, err := b.h.preflight(context.Background())
	require.NoError(t, err)
	require.NoError(t, b.h.startAndPrune(context.Background()))
	assert.Equal(t, 1, b.rec.count("site exec spire-server-0 -c spire-server -- /opt/spire/bin/spire-server agent evict"))
	assert.NotEqual(t, -1, b.rec.index("site exec spire-server-0 -c spire-server -- /opt/spire/bin/spire-server agent evict "+
		"-socketPath "+serverSocket+" -spiffeID "+seedAgent))
}

func TestRestartSiteAgents(t *testing.T) {
	tests := []struct {
		name            string
		agentsRestarted bool
		siteBundleStale bool
		wantPatches     int
		wantErr         string
	}{
		{name: "restarts the agents once the site publishes the seed authorities", wantPatches: 1},
		{name: "skips agents already restarted for this seed bundle", agentsRestarted: true},
		{name: "waits while the site bundle lacks the seed authorities", siteBundleStale: true, wantErr: "the site bundle ConfigMap to publish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fixture{t: t, agentsRestarted: tt.agentsRestarted, siteBundleStale: tt.siteBundleStale}.build()
			_, err := b.h.preflight(context.Background())
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			err = b.h.restartSiteAgents(ctx, [][]byte{b.authority}, fingerprint([][]byte{b.authority}))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantPatches, b.rec.count("site patch daemonset"))
		})
	}
}

func TestVerifyRefusesASiteWithoutTheSeedAuthorities(t *testing.T) {
	b := fixture{t: t}.build()
	err := b.h.verify(context.Background(), [][]byte{testCert(t)}, "marker")
	require.ErrorContains(t, err, "does not serve the seed's X.509 authorities")
	assert.Equal(t, -1, b.rec.index("site patch statefulset"), "no marker on failure")
}

func TestRepointNodes(t *testing.T) {
	stale := api.NodeSpireAgentStatus{Name: "node-b", Reachable: false, Message: ptr("node agent unreachable")}
	other := api.SpireServer{Address: "198.51.100.1", Port: 8081}
	target := api.SpireServer{Address: siteAddress, Port: sitePort}
	tests := []struct {
		name       string
		patchCode  int
		inProgress api.UpdateInProgressType
		requested  *api.SpireServer
		status     [][]api.NodeSpireAgentStatus
		failed     string
		wantErr    string
	}{
		{
			name: "every node moves", inProgress: api.UpdateInProgressTypeSpireServerUpdate,
			status: [][]api.NodeSpireAgentStatus{{stale}, {onSite("node-a"), onSite("node-b")}},
		},
		{
			name: "timeout lists the stragglers", inProgress: api.UpdateInProgressTypeSpireServerUpdate,
			status: [][]api.NodeSpireAgentStatus{{onSite("node-a"), stale}}, wantErr: "timed out waiting for nodes",
		},
		{
			name: "a failed update ends the wait", status: [][]api.NodeSpireAgentStatus{{onSite("node-a"), stale}},
			failed: "SPIRE server update failed on node-b", wantErr: "ended before every node moved: SPIRE server update failed on node-b",
		},
		{
			name: "an ended update without a failure lists the stragglers", status: [][]api.NodeSpireAgentStatus{{onSite("node-a"), stale}},
			wantErr: "ended before every node moved: node-b: node agent unreachable",
		},
		{
			name: "409 from the same SPIRE update is joined", patchCode: http.StatusConflict,
			inProgress: api.UpdateInProgressTypeSpireServerUpdate, requested: &target,
			status: [][]api.NodeSpireAgentStatus{{onSite("node-a")}},
		},
		{
			name: "409 from an update to another server fails", patchCode: http.StatusConflict,
			inProgress: api.UpdateInProgressTypeSpireServerUpdate, requested: &other,
			status: [][]api.NodeSpireAgentStatus{{onSite("node-a")}}, wantErr: "another operation is in progress",
		},
		{
			name: "409 from another operation fails", patchCode: http.StatusConflict,
			inProgress: api.UpdateInProgressTypeK8SUpgrade, requested: &target,
			status: [][]api.NodeSpireAgentStatus{{onSite("node-a")}}, wantErr: "another operation is in progress",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fixture{t: t}.build()
			b.controller.patchCode, b.controller.inProgress = tt.patchCode, tt.inProgress
			b.controller.requested, b.controller.status, b.controller.failed = tt.requested, tt.status, tt.failed
			err := b.h.repointNodes(context.Background())
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func ptr[T any](v T) *T { return &v }
