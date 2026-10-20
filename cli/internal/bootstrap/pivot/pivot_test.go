// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pivot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/spirehandover"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

const vip = "192.0.2.10"

// fakeRunner answers kubectl and clusterctl calls by the first rule whose
// substring the command line contains, and records every call. The fixture
// also records the patches the fake clients receive here, so one log holds
// the order of everything the pivot did.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	rules []rule
}

type rule struct {
	match string
	out   func() (string, error)
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.calls = append(r.calls, line)
	rules := r.rules
	r.mu.Unlock()
	for _, rl := range rules {
		if strings.Contains(line, rl.match) {
			return rl.out()
		}
	}
	return "", nil
}

func (r *fakeRunner) record(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, line)
}

func (r *fakeRunner) on(match, out string) {
	r.rules = append(r.rules, rule{match, func() (string, error) { return out, nil }})
}

// index returns the position of the first call containing match, or -1.
func (r *fakeRunner) index(match string) int {
	for i, c := range r.calls {
		if strings.Contains(c, match) {
			return i
		}
	}
	return -1
}

func deployment(ns, name, image string, annotations map[string]string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image}}}},
		},
	}
}

func clusterObjects(extra ...runtime.Object) []runtime.Object {
	return append([]runtime.Object{
		deployment(controllerNamespace, controllerName, "controller:1", nil),
		deployment("capi-system", "capi-controller-manager", "capi:1", nil),
		deployment("capnico-system", "controller-manager", "capnico:1", nil),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: postgresNamespace, Name: "postgres-0",
				Labels: map[string]string{"app.kubernetes.io/name": "postgresql"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: spireNamespace, Name: spireExternalSvc},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{NodePort: 30440}}},
		},
	}, extra...)
}

// capiObject builds an object of the Cluster API kind or CRD at gvr.
func capiObject(gvr schema.GroupVersionResource, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: fields}
	if obj.Object == nil {
		obj.Object = map[string]any{}
	}
	obj.SetAPIVersion(gvr.GroupVersion().String())
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

// capiCluster is the Cluster API Cluster "site", with its ControlPlaneInitialized condition at initialized.
func capiCluster(initialized string) *unstructured.Unstructured {
	return capiObject(capiClusters, "Cluster", managementNamespace, "site", map[string]any{"status": map[string]any{
		"conditions": []any{map[string]any{
			"type": "ControlPlaneInitialized", "status": initialized, "reason": "Test", "lastTransitionTime": "2026-01-01T00:00:00Z",
		}},
	}})
}

// capiMachine is a Machine of the cluster "site", linked to node unless node is empty.
func capiMachine(name, node string) *unstructured.Unstructured {
	m := capiObject(capiMachines, "Machine", managementNamespace, name, nil)
	m.SetLabels(map[string]string{clusterNameLabel: "site"})
	if node != "" {
		m.Object["status"] = map[string]any{"nodeRef": map[string]any{"name": node}}
	}
	return m
}

func crd(name string) *unstructured.Unstructured {
	return capiObject(crds, "CustomResourceDefinition", "", name, nil)
}

func newDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		capiClusters: "ClusterList", capiMachines: "MachineList", crds: "CustomResourceDefinitionList",
	}, objs...)
}

type fixture struct {
	p       *pivot
	runner  *fakeRunner
	seed    *fake.Clientset
	site    *fake.Clientset
	seedDyn *dynamicfake.FakeDynamicClient
	siteDyn *dynamicfake.FakeDynamicClient
	// handedOver records the SPIRE handover's config.
	handedOver *spirehandover.Config
}

// newFixture builds a pivot over fake clusters. seedCAPI and siteCAPI are the
// Cluster API objects and CRDs each holds.
func newFixture(t *testing.T, seedObjs, siteObjs, seedCAPI, siteCAPI []runtime.Object) *fixture {
	t.Helper()
	f := &fixture{
		runner: &fakeRunner{}, seed: fake.NewClientset(seedObjs...), site: fake.NewClientset(siteObjs...),
		seedDyn: newDynamic(seedCAPI...), siteDyn: newDynamic(siteCAPI...),
	}
	record := func(side string) clienttesting.ReactionFunc {
		return func(a clienttesting.Action) (bool, runtime.Object, error) {
			pa := a.(clienttesting.PatchAction)
			f.runner.record(fmt.Sprintf("%s patch %s %s %s", side, pa.GetResource().Resource, pa.GetName(), pa.GetPatch()))
			return false, nil, nil
		}
	}
	f.seed.PrependReactor("patch", "*", record("seed"))
	f.site.PrependReactor("patch", "*", record("site"))
	f.seedDyn.PrependReactor("patch", "*", record("seed"))
	f.siteDyn.PrependReactor("patch", "*", record("site"))
	f.p = newPivot(Config{
		ClusterName: "site", SiteSpireAddress: vip, PollInterval: time.Millisecond,
		Runner: f.runner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	},
		&cluster{name: "seed", kubeconfig: "seed.kubeconfig", kube: f.seed, dyn: f.seedDyn},
		&cluster{name: "site", kubeconfig: "site.kubeconfig", kube: f.site, dyn: f.siteDyn})
	f.p.handover = func(_ context.Context, cfg spirehandover.Config) error {
		f.handedOver = &cfg
		return nil
	}
	return f
}

// readyToMove is what the seed holds when its cluster can be moved.
func readyToMove() []runtime.Object {
	return []runtime.Object{
		capiCluster("True"), capiMachine("site-cp-a", "site-cp-a"),
		crd("nicomachines.infrastructure.cluster.x-k8s.io"), crd("machines.cluster.x-k8s.io"),
	}
}

// happyPath makes every check answer as on a cluster ready to pivot.
func (f *fixture) happyPath() {
	f.runner.rules = append(f.runner.rules,
		rule{"workflow_outbox", func() (string, error) { return "0", nil }},
		rule{"temporal workflow count", func() (string, error) { return "Total: 0", nil }},
		rule{"clusterctl move", func() (string, error) {
			return "", f.seedDyn.Tracker().Delete(capiClusters, managementNamespace, "site")
		}},
	)
	restored := false
	f.runner.rules = append(f.runner.rules,
		rule{"pg_restore", func() (string, error) { restored = true; return "", nil }},
		rule{"seed.kubeconfig -n postgres exec postgres-0 -- psql", func() (string, error) { return "cluster a\nnode b", nil }},
		rule{"site.kubeconfig -n postgres exec postgres-0 -- psql", func() (string, error) {
			if restored {
				return "cluster a\nnode b", nil
			}
			return "", nil
		}},
		rule{"get --raw", func() (string, error) {
			addr := vip
			b, _ := json.Marshal(api.ClusterSpireServerStatus{Nodes: []api.NodeSpireAgentStatus{
				{Name: "n1", Reachable: true, HasValidSVID: true, ServerAddress: &addr},
			}})
			return string(b), nil
		}},
	)
}

func TestRunPivotsInOrder(t *testing.T) {
	etcdKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: controllerNamespace, Name: etcdKeyPrefix + "x"},
		Data:       map[string][]byte{"active": []byte("k")},
	}
	f := newFixture(t, clusterObjects(etcdKey), clusterObjects(), readyToMove(), nil)
	f.happyPath()

	require.NoError(t, f.p.run(context.Background()))

	require.NotNil(t, f.handedOver)
	require.Equal(t, vip, f.handedOver.SiteSpireAddress)
	require.Equal(t, 30440, f.handedOver.SiteSpirePort)

	order := []string{
		`site patch deployments nvke-controller {"spec":{"replicas":0}}`,
		`seed patch deployments nvke-controller {"spec":{"replicas":0}}`,
		"seed patch customresourcedefinitions nicomachines.infrastructure.cluster.x-k8s.io",
		"clusterctl move --kubeconfig seed.kubeconfig --to-kubeconfig site.kubeconfig --namespace nvke-system",
		"pg_dump",
		"pg_restore",
		`site patch deployments nvke-controller {"spec":{"replicas":1}}`,
		"get --raw",
	}
	last := -1
	for _, call := range order {
		i := f.runner.index(call)
		require.Greater(t, i, last, "%q out of order in %v", call, f.runner.calls)
		last = i
	}
	require.Equal(t, -1, f.runner.index("patch customresourcedefinitions machines.cluster"),
		"only infrastructure CRDs are labelled")

	copied, err := f.site.CoreV1().Secrets(controllerNamespace).Get(context.Background(), etcdKeyPrefix+"x", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []byte("k"), copied.Data["active"])
}

func TestQuiesceSeedFailsWhenWorkArrivesWhileStopping(t *testing.T) {
	f := newFixture(t, clusterObjects(), clusterObjects(), nil, nil)
	f.runner.rules = append(f.runner.rules,
		rule{"workflow_outbox", func() (string, error) {
			if f.runner.index(`seed patch deployments nvke-controller {"spec":{"replicas":0}}`) != -1 {
				return "1", nil
			}
			return "0", nil
		}},
		rule{"temporal workflow count", func() (string, error) { return "Total: 0", nil }},
	)
	require.ErrorContains(t, f.p.quiesceSeed(context.Background()), "took on work while stopping (1 queued, 0 running)")
}

func TestStopControllerWaitsForTerminatingPods(t *testing.T) {
	terminating := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: controllerNamespace, Name: "terminating-controller", Labels: map[string]string{"app": controllerName},
	}}
	f := newFixture(t, clusterObjects(terminating), clusterObjects(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorContains(t, f.p.stopController(ctx, f.p.seed), "seed controller pods to stop")

	require.NoError(t, f.seed.CoreV1().Pods(controllerNamespace).Delete(context.Background(), terminating.Name, metav1.DeleteOptions{}))
	require.NoError(t, f.p.stopController(context.Background(), f.p.seed))
}

func TestCheckCAPI(t *testing.T) {
	t.Run("refuses when the seed does not hold the cluster", func(t *testing.T) {
		f := newFixture(t, clusterObjects(), clusterObjects(), nil, []runtime.Object{capiCluster("True")})
		require.ErrorContains(t, f.p.checkCAPI(context.Background()), "the seed does not hold the Cluster API Cluster")
	})
	t.Run("waits until every Machine has a node", func(t *testing.T) {
		f := newFixture(t, clusterObjects(), clusterObjects(),
			[]runtime.Object{capiCluster("True"), capiMachine("site-cp-a", "site-cp-a"), capiMachine("site-cp-b", "")}, nil)
		polls := 0
		f.seedDyn.PrependReactor("list", "machines", func(clienttesting.Action) (bool, runtime.Object, error) {
			if polls++; polls == 3 {
				return false, nil, f.seedDyn.Tracker().Update(capiMachines, capiMachine("site-cp-b", "site-cp-b"), managementNamespace)
			}
			return false, nil, nil
		})
		require.NoError(t, f.p.checkCAPI(context.Background()))
		require.Equal(t, 3, polls)
	})
	t.Run("does not move before the control plane is initialized", func(t *testing.T) {
		f := newFixture(t, clusterObjects(), clusterObjects(),
			[]runtime.Object{capiCluster("False"), capiMachine("site-cp-a", "site-cp-a")}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		require.ErrorContains(t, f.p.checkCAPI(ctx), "control plane")
	})
	t.Run("refuses different provider versions", func(t *testing.T) {
		f := newFixture(t, clusterObjects(),
			[]runtime.Object{
				deployment("capi-system", "capi-controller-manager", "capi:1", nil),
				deployment("capnico-system", "controller-manager", "capnico:2", nil),
			}, []runtime.Object{capiCluster("True")}, nil)
		require.ErrorContains(t, f.p.checkCAPI(context.Background()), "capnico:1 on the seed but capnico:2 on the site")
	})
}

func TestRunChecksCAPIBeforeStoppingAnything(t *testing.T) {
	f := newFixture(t, clusterObjects(),
		[]runtime.Object{
			deployment("capi-system", "capi-controller-manager", "capi:1", nil),
			deployment("capnico-system", "controller-manager", "capnico:2", nil),
		}, readyToMove(), nil)
	f.happyPath()

	require.ErrorContains(t, f.p.run(context.Background()), "capnico:1 on the seed but capnico:2 on the site")
	require.Equal(t, -1, f.runner.index("patch deployments"), "no controller is scaled: %v", f.runner.calls)
	require.Nil(t, f.handedOver)
}

func TestMoveDatabase(t *testing.T) {
	t.Run("fails when the restore does not match", func(t *testing.T) {
		f := newFixture(t, clusterObjects(), clusterObjects(), nil, nil)
		f.runner.on("seed.kubeconfig -n postgres exec postgres-0 -- psql", "cluster a")
		f.runner.on("site.kubeconfig -n postgres exec postgres-0 -- psql", "")
		require.ErrorContains(t, f.p.moveDatabase(context.Background()), "after the restore")
		require.NotEqual(t, -1, f.runner.index("rm -f "+dumpPath), "the dumps are removed")
	})
}

func TestRunWithMTLSDisabledSkipsSpire(t *testing.T) {
	f := newFixture(t, clusterObjects(), clusterObjects(), readyToMove(), nil)
	f.p.cfg.DisableMTLS = true
	f.happyPath()

	require.NoError(t, f.p.run(context.Background()))

	require.Nil(t, f.handedOver, "no SPIRE handover without mTLS")
	require.Equal(t, -1, f.runner.index("get --raw"), "no SVID check without mTLS")
	require.NotEqual(t, -1, f.runner.index("clusterctl move"))
	require.NotEqual(t, -1, f.runner.index("pg_restore"))
}

func TestActivateSiteScalesTheControllerToOne(t *testing.T) {
	site := deployment(controllerNamespace, controllerName, "controller:1", nil)
	zero := int32(0)
	site.Spec.Replicas = &zero
	f := newFixture(t, clusterObjects(), []runtime.Object{site}, nil, nil)
	require.NoError(t, f.p.activateSite(context.Background()))
	d, err := f.site.AppsV1().Deployments(controllerNamespace).Get(context.Background(), controllerName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, int32(1), *d.Spec.Replicas)
}

func TestVerifyListsNodesNotOnTheSite(t *testing.T) {
	f := newFixture(t, clusterObjects(), clusterObjects(), nil, nil)
	other := "198.51.100.1"
	b, err := json.Marshal(api.ClusterSpireServerStatus{Nodes: []api.NodeSpireAgentStatus{
		{Name: "n1", Reachable: true, HasValidSVID: true, ServerAddress: &other},
	}})
	require.NoError(t, err)
	f.runner.on("get --raw", string(b))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorContains(t, f.p.verify(ctx), "n1")
}

func TestCopySecretsUpdatesASecretAlreadyOnTheSite(t *testing.T) {
	key := func(data string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: controllerNamespace, Name: etcdKeyPrefix + "x", Labels: labels},
			Data:       map[string][]byte{"active": []byte(data)},
		}
	}
	f := newFixture(t, clusterObjects(key("rotated", nil)), clusterObjects(key("stale", map[string]string{"site": "kept"})), nil, nil)

	require.NoError(t, f.p.copySecrets(context.Background()))

	got, err := f.site.CoreV1().Secrets(controllerNamespace).Get(context.Background(), etcdKeyPrefix+"x", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []byte("rotated"), got.Data["active"], "the site takes the seed's data")
	require.Equal(t, "kept", got.Labels["site"], "the site's metadata is kept")
}
