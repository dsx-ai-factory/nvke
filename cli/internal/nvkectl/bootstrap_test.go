// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/controllerdeploy"
	bootstrapengine "github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/engine"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitecluster"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// fakeCWERunner is a scripted controllerdeploy.CommandRunner: every helm call
// succeeds, and every `kubectl get --raw .../readyz` call reports healthy
// immediately, so runBootstrapSite's controller deploy step can be exercised
// without a real helm binary, kubectl binary or cluster.
type fakeCWERunner struct {
	kubeconfig []byte
}

func newFakeCWERunner(t *testing.T) fakeCWERunner {
	t.Helper()
	return fakeCWERunner{kubeconfig: fakeSiteKubeconfig(t)}
}

func (r fakeCWERunner) Run(_ context.Context, name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if name == "kubectl" && strings.Contains(joined, "jsonpath={.data.value}") {
		// The Secret the controller publishes the finished cluster's kubeconfig in.
		kc := r.kubeconfig
		if len(kc) == 0 {
			kc = []byte("apiVersion: v1\nkind: Config\n")
		}
		return base64.StdEncoding.EncodeToString(kc), nil
	}
	if name == "kubectl" && strings.Contains(joined, "get secret etcd-encryption-key-") {
		// The Secret the controller keeps the cluster's etcd encryption key in.
		return `{"apiVersion":"v1","kind":"Secret","data":{"active":"` +
			base64.StdEncoding.EncodeToString([]byte(fakeEtcdKey)) + `","active-since":"MQ=="}}`, nil
	}
	if name == "kubectl" && strings.Contains(joined, "readyz") {
		return `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`, nil
	}
	if name == "kubectl" && strings.Contains(joined, "capi-controller-manager") {
		// CAPI is not installed yet, so controllerdeploy runs clusterctl.
		return "Error from server (NotFound)", errors.New("exit status 1")
	}
	return "", nil
}

const fakeBreakglassCertPEM = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

var kubeAPICodecs = serializer.NewCodecFactory(scheme.Scheme)

// fakeSiteKubeconfig is an admin.conf that points at an httptest kube-apiserver.
// That server signs kube-apiserver-client CSRs so bootstrap can run
// GenerateBreakglassKubeconfig without a cluster.
func fakeSiteKubeconfig(t *testing.T) []byte {
	t.Helper()

	var mu sync.Mutex
	bindings := map[string]struct{}{}
	csrs := map[string]*certificatesv1.CertificateSigningRequest{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /apis/rbac.authorization.k8s.io/v1/clusterrolebindings", func(w http.ResponseWriter, r *http.Request) {
		var b rbacv1.ClusterRoleBinding
		if err := decodeK8sObject(r, &b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := bindings[b.Name]; ok {
			writeK8sObject(w, r, http.StatusConflict, &metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   metav1.StatusFailure,
				Reason:   metav1.StatusReasonAlreadyExists,
				Code:     http.StatusConflict,
				Message:  b.Name + " already exists",
			})
			return
		}
		bindings[b.Name] = struct{}{}
		writeK8sObject(w, r, http.StatusCreated, &b)
	})
	mux.HandleFunc("POST /apis/certificates.k8s.io/v1/certificatesigningrequests", func(w http.ResponseWriter, r *http.Request) {
		var csr certificatesv1.CertificateSigningRequest
		if err := decodeK8sObject(r, &csr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		csrs[csr.Name] = csr.DeepCopy()
		mu.Unlock()
		writeK8sObject(w, r, http.StatusCreated, &csr)
	})
	mux.HandleFunc("PUT /apis/certificates.k8s.io/v1/certificatesigningrequests/{name}/approval", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var csr certificatesv1.CertificateSigningRequest
		if err := decodeK8sObject(r, &csr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		stored, ok := csrs[name]
		if !ok {
			mu.Unlock()
			http.NotFound(w, r)
			return
		}
		stored.Status.Conditions = csr.Status.Conditions
		stored.Status.Certificate = []byte(fakeBreakglassCertPEM)
		csrs[name] = stored
		out := stored.DeepCopy()
		mu.Unlock()
		writeK8sObject(w, r, http.StatusOK, out)
	})
	mux.HandleFunc("GET /apis/certificates.k8s.io/v1/certificatesigningrequests/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		mu.Lock()
		stored, ok := csrs[name]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeK8sObject(w, r, http.StatusOK, stored)
	})
	mux.HandleFunc("DELETE /apis/certificates.k8s.io/v1/certificatesigningrequests/{name}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(csrs, r.PathValue("name"))
		mu.Unlock()
		writeK8sObject(w, r, http.StatusOK, &metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status:   metav1.StatusSuccess,
			Code:     http.StatusOK,
		})
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   srv.URL,
		CertificateAuthorityData: ca,
	}
	cfg.AuthInfos["kubernetes-admin"] = &clientcmdapi.AuthInfo{Token: "test"}
	cfg.Contexts["admin"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "kubernetes-admin"}
	cfg.CurrentContext = "admin"
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("write fake site kubeconfig: %v", err)
	}
	return raw
}

func decodeK8sObject(r *http.Request, into runtime.Object) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_, _, err = kubeAPICodecs.UniversalDeserializer().Decode(body, nil, into)
	return err
}

func writeK8sObject(w http.ResponseWriter, r *http.Request, code int, obj runtime.Object) {
	mediaType := runtime.ContentTypeJSON
	if strings.Contains(r.Header.Get("Accept"), runtime.ContentTypeProtobuf) {
		mediaType = runtime.ContentTypeProtobuf
	}
	info, ok := runtime.SerializerInfoForMediaType(kubeAPICodecs.SupportedMediaTypes(), mediaType)
	if !ok {
		http.Error(w, "no serializer for "+mediaType, http.StatusInternalServerError)
		return
	}
	if gvks, _, err := scheme.Scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
		obj.GetObjectKind().SetGroupVersionKind(gvks[0])
	}
	w.Header().Set("Content-Type", info.MediaType)
	w.WriteHeader(code)
	_ = info.Serializer.Encode(obj, w)
}

// startFakeCWEAPI stands up an HTTP server that answers the two calls
// `bootstrap site` makes after the deploy — create the cluster, then poll it —
// and points nvkectl at it through the port it dials.
//
// A real server rather than an injected client: the address is the thing under
// test on this path. nvkectl builds it from the host port the controller NodePort was
// mapped to, and a client handed in by a test would prove nothing about that.
func startFakeCWEAPI(t *testing.T, provisioned bool) {
	t.Helper()
	joined := 0
	if provisioned {
		joined = 1
	}
	startFakeCWEAPIWithNodes(t, provisioned, 1, joined)
}

// startFakeCWEAPIWithNodes is startFakeCWEAPI with the control-plane node list
// spelled out: total nodes, of which the first joined have finished
// provisioning. It is how a test holds a control plane short of the count the
// site spec asked for, which is the state the cluster's own Provisioned
// condition cannot distinguish from a finished one.
func startFakeCWEAPIWithNodes(t *testing.T, provisioned bool, total, joined int) {
	t.Helper()

	id := uuid.New()
	status := api.ConditionStatusTrue
	if !provisioned {
		status = api.ConditionStatusFalse
	}
	cluster := api.Cluster{
		ID:   id,
		Name: "nvke-site",
		Conditions: api.ClusterConditions{
			Provisioned:  api.Condition{Status: status},
			Provisioning: api.Condition{Status: api.ConditionStatusTrue},
			Deleting:     api.Condition{Status: api.ConditionStatusFalse},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/clusters", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cluster)
	})
	mux.HandleFunc("GET /api/clusters/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cluster)
	})
	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, _ *http.Request) {
		nodes := make([]api.Node, 0, total)
		for i := range total {
			status, message := api.ConditionStatusFalse, "Joining node"
			if i < joined {
				status, message = api.ConditionStatusTrue, "Provisioned node"
			}
			nodes = append(nodes, api.Node{
				Name:         fmt.Sprintf("node%d", i+1),
				ControlPlane: true,
				Conditions: api.NodeConditions{
					Provisioned: api.Condition{Status: status, Message: message},
				},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ListNodesResponse{Nodes: nodes})
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake controller API: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parsing the test server address %q: %v", srv.URL, err)
	}
	t.Setenv("NVKECTL_NVKE_CONTROLLER_PORT", port)
}

// fakeBootstrapRunner is a scripted bootstrapengine.CommandRunner so
// runBootstrapSite can be exercised without a real kind binary or container
// runtime. It answers every invocation the happy path issues: no existing
// cluster, a successful create, and a resolvable host manifest digest.
type fakeBootstrapRunner struct{}

func (fakeBootstrapRunner) Run(_ context.Context, _ map[string]string, name string, args ...string) (string, error) {
	if len(args) < 2 {
		return "", nil
	}
	if name == "kind" && args[0] == "get" && args[1] == "clusters" {
		// Mirrors kind's own exit-1-with-message behavior when no
		// clusters exist, which bootstrapengine.Exists treats as
		// "false, nil" rather than a real error.
		return "No kind clusters found.\n", errors.New("exit status 1")
	}
	if args[0] == "image" && args[1] == "inspect" {
		// Target digest and host manifest digest, equal: nothing to flatten.
		d := "sha256:" + strings.Repeat("a", 64)
		return d + " " + d + "\n", nil
	}
	return "", nil
}

const validSiteSpec = `
clusterName: nvke-site
cluster:
  nodes:
    - name: node1
      ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
`

const threeNodeSiteSpec = `
clusterName: nvke-site
cluster:
  nodes:
    - name: node1
      ip: 10.0.0.1
    - name: node2
      ip: 10.0.0.2
    - name: node3
      ip: 10.0.0.3
  controlPlaneVIP: 10.0.0.100
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
`

// TestBootstrapSiteCommandWired verifies the command tree exposes
// "nvkectl bootstrap site" — the runnable command required by the ticket.
func TestBootstrapSiteCommandWired(t *testing.T) {
	root := NewRootCmd("test", "test", "test", "test")
	cmd, _, err := root.Find([]string{"bootstrap", siteSubcommand})
	if err != nil {
		t.Fatalf("find bootstrap site: %v", err)
	}
	if cmd.Name() != siteSubcommand {
		t.Fatalf("resolved command = %q, want %q", cmd.Name(), siteSubcommand)
	}
}

// TestBootstrapSiteImageFlags pins the two image flags by name, so the
// registration cannot drift from the help text and the docs that name them.
func TestBootstrapSiteImageFlags(t *testing.T) {
	root := NewRootCmd("test", "test", "test", "test")
	cmd, _, err := root.Find([]string{"bootstrap", siteSubcommand})
	if err != nil {
		t.Fatalf("find bootstrap site: %v", err)
	}
	for _, name := range []string{"nvke-controller-image", "provisioned-provider-image"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("bootstrap site has no --%s flag", name)
		}
	}
	for _, name := range []string{"controller-image", "cwe-image"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("bootstrap site still registers --%s", name)
		}
	}
}

// TestRunBootstrapSiteDryRunValid confirms a valid site spec passes validation
// and dry-run returns no error without dispatching anything.
func TestRunBootstrapSiteDryRunValid(t *testing.T) {
	path := writeSiteSpec(t, validSiteSpec)
	if err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{siteSpec: path, dryRun: true}); err != nil {
		t.Fatalf("dry-run of valid site spec returned error: %v", err)
	}
}

// TestRunBootstrapSiteInvalidFailsFast confirms an invalid site spec errors
// before reaching the (stubbed) dispatch step.
func TestRunBootstrapSiteInvalidFailsFast(t *testing.T) {
	const bad = `
clusterName: nvke-site
cluster:
  nodes:
    - name: node1
      ip: not-an-ip
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
`
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{siteSpec: writeSiteSpec(t, bad)})
	if err == nil {
		t.Fatal("invalid site spec did not error")
	}
	if strings.Contains(err.Error(), "creating cluster") {
		t.Errorf("reached the create call before validation failed: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster.nodes[0].ip") {
		t.Errorf("error does not name the bad field: %v", err)
	}
}

// TestRunBootstrapSiteCreatesTheCluster is the whole command on its happy
// path: a valid site spec passes validation, the ephemeral bootstrap kind
// cluster is stood up and torn down again, the controller is asked to create the cluster,
// and the finished cluster's kubeconfig is written out.
//
// Every binary is faked, so this needs no kind, no container runtime and no
// cluster; the controller API is a real HTTP server on the port nvkectl dials.
func TestRunBootstrapSiteCreatesTheCluster(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nvke-site.kubeconfig")
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	spireEntriesDumped := false
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       kubeconfigOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(_ context.Context, source, outputDir string) (int, error) {
			spireEntriesDumped = true
			if source == "" {
				t.Error("source kubeconfig path is empty")
			}
			if outputDir != filepath.Dir(kubeconfigOut) {
				t.Errorf("output directory = %q, want %q", outputDir, filepath.Dir(kubeconfigOut))
			}
			return 1, nil
		},
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
	})
	if err != nil {
		t.Fatalf("runBootstrapSite returned error: %v", err)
	}

	if !runner.sawArgs("create", "cluster") {
		t.Errorf("runBootstrapSite did not create the bootstrap kind cluster; calls: %v", runner.calls)
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("runBootstrapSite did not tear down the bootstrap kind cluster; calls: %v", runner.calls)
	}
	if !spireEntriesDumped {
		t.Error("runBootstrapSite did not dump SPIRE bootstrap entries")
	}
	body, readErr := os.ReadFile(kubeconfigOut)
	if readErr != nil {
		t.Fatalf("no kubeconfig written for the finished cluster: %v", readErr)
	}
	if !strings.Contains(string(body), "kind: Config") {
		t.Errorf("the written kubeconfig is not one: %q", body)
	}
}

func TestRunBootstrapSiteWritesIssuedBreakglassKubeconfig(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nvke-site.kubeconfig")
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     fakeBootstrapRunner{},
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       kubeconfigOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries:    func(context.Context, string, string) (int, error) { return 0, nil },
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err != nil {
		t.Fatalf("runBootstrapSite: %v", err)
	}
	body, readErr := os.ReadFile(kubeconfigOut)
	if readErr != nil {
		t.Fatalf("read kubeconfig: %v", readErr)
	}
	cfg, loadErr := clientcmd.Load(body)
	if loadErr != nil {
		t.Fatalf("parse kubeconfig: %v", loadErr)
	}
	if cfg.CurrentContext != "nvke-breakglass" {
		t.Errorf("current-context = %q, want nvke-breakglass", cfg.CurrentContext)
	}
	user := cfg.AuthInfos["nvke-breakglass"]
	if user == nil {
		t.Fatal("issued kubeconfig has no nvke-breakglass user")
	}
	if user.Token != "" {
		t.Error("issued kubeconfig kept the admin token")
	}
	if len(user.ClientCertificateData) == 0 || len(user.ClientKeyData) == 0 {
		t.Error("issued kubeconfig is missing the minted client cert or key")
	}
}

func TestRunBootstrapSiteRetainsBootstrapClusterWhenSpireEntryDumpFails(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, errors.New("dump failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "writing SPIRE bootstrap resources") {
		t.Fatalf("error = %v, want SPIRE dump failure", err)
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after the SPIRE copy failed")
	}
}

func TestRunBootstrapSiteSkipsSpireEntryDumpWhenMTLSDisabled(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	dumpCalled := false
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		disableMTLS:         true,
		bootstrapRunner:     fakeBootstrapRunner{},
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			dumpCalled = true
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite returned error: %v", err)
	}
	if dumpCalled {
		t.Error("SPIRE entries were dumped with mTLS disabled")
	}
}

// TestRunBootstrapSiteFailsWhenTheClusterNeverComesUp: the command's promise is
// a working cluster, so a cluster that never reports provisioned has to be a
// failure rather than a command that returns 0 and leaves the operator to find
// out later.
func TestRunBootstrapSiteFailsWhenTheClusterNeverComesUp(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, false)
	t.Setenv("NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT", "5ms")

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		clusterTimeout:      5 * time.Millisecond,
		kubeconfigOut:       filepath.Join(t.TempDir(), "unused.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err == nil {
		t.Fatal("expected an error when the cluster never reports provisioned")
	}
	if !strings.Contains(err.Error(), "did not report provisioned") {
		t.Errorf("error %q does not say the cluster never came up", err)
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after site cluster creation started")
	}
	if !strings.Contains(err.Error(), "retained for recovery") || !strings.Contains(err.Error(), "kubeconfig:") {
		t.Errorf("error does not explain how to recover from the retained cluster: %v", err)
	}
}

func TestRunBootstrapSiteRetainsClusterWhenKubeconfigIsNotPublished(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           missingKubeconfigRunner{},
		kubeconfigTimeout:   5 * time.Millisecond,
		kubeconfigOut:       filepath.Join(t.TempDir(), "unused.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err == nil {
		t.Fatal("expected an error when the kubeconfig Secret is not published")
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after the site cluster became provisioned")
	}
	// The env prefix specifically, not just the command: without it the
	// operator's copy-pasted `kind delete cluster` resolves through kind's
	// Docker default, reports the cluster as missing, exits 0, and leaves the
	// cluster behind for the next run to collide with.
	for _, want := range []string{
		"not published",
		"retained for recovery",
		"KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// runtimeOnPath stands in for exec.LookPath finding the container runtime, so
// tests do not require a real nerdctl installation on the machine running
// `go test`. Tests about the preflight itself supply their own stub.
func runtimeOnPath(name string) (string, error) { return "/usr/local/bin/" + name, nil }

// setBootstrapEnv fills in the images `bootstrap site` requires, so a test
// about something else does not fail on one of them.
func setBootstrapEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", "nvcr.io/org/controller:edge")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "cluster-api-provider-provisioned:latest")
}

func TestCWEDeployConfigUsesSiteSpecTrustDomain(t *testing.T) {
	setBootstrapEnv(t)
	t.Setenv("NVKECTL_SPIFFE_TRUST_DOMAIN", "stale.example.com")

	cfg, err := cweDeployConfig(&sitespec.SiteSpec{
		Spire: sitespec.Spire{TrustDomain: "site.example.com"},
	}, "bootstrap-cluster", false, "", "")
	if err != nil {
		t.Fatalf("cweDeployConfig: %v", err)
	}
	if cfg.SPIFFETrustDomain != "site.example.com" {
		t.Errorf("SPIFFETrustDomain = %q, want site-spec value %q", cfg.SPIFFETrustDomain, "site.example.com")
	}
}

func TestSiteDeployConfigNamesTheSite(t *testing.T) {
	seed := controllerdeploy.Config{
		CWEImage: "registry.example.com/nvke-controller:1", SPIFFEClusterName: "nvkectl-bootstrap",
		SPIFFETrustDomain: "site.example.com", SPIREExternalAddress: "192.0.2.1",
	}
	inv := &sitespec.SiteSpec{ClusterName: "site", Cluster: sitespec.Cluster{ControlPlaneVIP: "192.0.2.100"}}
	cfg := siteDeployConfig(inv, seed)
	if cfg.SPIFFEClusterName != "site" || cfg.SPIREExternalAddress != "192.0.2.100" {
		t.Errorf("SPIFFEClusterName, SPIREExternalAddress = %q, %q; want the site's name and VIP",
			cfg.SPIFFEClusterName, cfg.SPIREExternalAddress)
	}
	if cfg.SPIFFETrustDomain != seed.SPIFFETrustDomain || cfg.CWEImage != seed.CWEImage {
		t.Errorf("the site must keep the bootstrap cluster's trust domain and image, got %q and %q",
			cfg.SPIFFETrustDomain, cfg.CWEImage)
	}
}

func TestRequireSitePullableImages(t *testing.T) {
	tests := []struct {
		name     string
		cfg      controllerdeploy.Config
		wantBare []string
	}{
		{
			name: "every image names a registry",
			cfg: controllerdeploy.Config{
				CWEImage:      "registry.example.com/nvke-controller:1",
				ProfilesImage: "registry.example.com/profiles:1", NicoInfrastructure: true,
			},
		},
		{
			name:     "a bare controller image",
			cfg:      controllerdeploy.Config{CWEImage: "nvke-controller:1", NicoInfrastructure: true},
			wantBare: []string{"nvke-controller:1"},
		},
		{
			name: "a bare profiles image",
			cfg: controllerdeploy.Config{
				CWEImage:      "registry.example.com/nvke-controller:1",
				ProfilesImage: "profiles:e2e", NicoInfrastructure: true,
			},
			wantBare: []string{"profiles:e2e"},
		},
		{
			name:     "every image bare",
			cfg:      controllerdeploy.Config{CWEImage: "nvke-controller:1", ProfilesImage: "profiles:e2e", NicoInfrastructure: true},
			wantBare: []string{"nvke-controller:1", "profiles:e2e"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireSitePullableImages(tt.cfg)
			if len(tt.wantBare) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "name no registry: "+strings.Join(tt.wantBare, ", ")+".")
		})
	}
}

func TestCWEDeployConfigDefaultsTrustDomain(t *testing.T) {
	setBootstrapEnv(t)
	t.Setenv("NVKECTL_SPIFFE_TRUST_DOMAIN", "")

	cfg, err := cweDeployConfig(&sitespec.SiteSpec{}, "bootstrap-cluster", false, "", "")
	if err != nil {
		t.Fatalf("cweDeployConfig: %v", err)
	}
	if cfg.SPIFFETrustDomain != "bootstrap-cluster.nke.local" {
		t.Errorf("SPIFFETrustDomain = %q, want derived value %q", cfg.SPIFFETrustDomain, "bootstrap-cluster.nke.local")
	}
}

// The image lookup must run against the runtime kind loads from, and a
// stale NVKECTL_BOOTSTRAP_PROVIDER export must not steer it back to a Docker
// daemon a host is no longer required to have.
func TestRunBootstrapSiteUsesNerdctl(t *testing.T) {
	setBootstrapEnv(t)
	t.Setenv("NVKECTL_BOOTSTRAP_PROVIDER", "docker")
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	_ = runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})

	if !slices.Contains(runner.names, bootstrapengine.RuntimeCLI) {
		t.Errorf("no %s call; ran %v", bootstrapengine.RuntimeCLI, runner.names)
	}
	if slices.Contains(runner.names, "docker") {
		t.Errorf("shelled out to docker; ran %v", runner.names)
	}
}

// A Docker-only host must be told what it is missing, through the command
// itself rather than through a unit-level call. Without the preflight the
// first failure is kind's — "failed to list clusters", several steps in,
// with the runtime named only in nested output — because kind execs nerdctl
// internally rather than this process doing it.
func TestRunBootstrapSiteRequiresNerdctlOnPath(t *testing.T) {
	setBootstrapEnv(t)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:        writeSiteSpec(t, validSiteSpec),
		bootstrapRunner: runner,
		cweRunner:       newFakeCWERunner(t),
		lookPath: func(name string) (string, error) {
			return "", fmt.Errorf("exec: %q: %w", name, exec.ErrNotFound)
		},
	})
	if err == nil {
		t.Fatal("expected an error when nerdctl is not on PATH")
	}
	if !strings.Contains(err.Error(), bootstrapengine.RuntimeCLI) {
		t.Errorf("error %q does not name the missing runtime", err)
	}
	if !strings.Contains(err.Error(), "Docker is not supported") {
		t.Errorf("error %q does not say Docker is unsupported", err)
	}
	// Before the cluster, not after: the point is to fail without having
	// stood anything up.
	if runner.sawArgs("create", "cluster") {
		t.Errorf("a missing runtime should be caught before the cluster is built; calls: %v", runner.calls)
	}
}

// An absent nerdctl is the first thing a Docker-only host hits, and pulling
// cannot fix it. The error has to name the requirement rather than blaming a
// registry login the operator has already done.
func TestEnsureImageAvailableDiagnosesMissingNerdctl(t *testing.T) {
	engine := bootstrapengine.New(runnerFunc(
		func(_ context.Context, _ map[string]string, _ string, _ ...string) (string, error) {
			return "", fmt.Errorf("exec %q: %w", bootstrapengine.RuntimeCLI, exec.ErrNotFound)
		},
	), nil)

	err := ensureImageAvailable(context.Background(), engine,
		&bootstrapengine.Cluster{Name: "c"}, "nvcr.io/org/controller:edge", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error when nerdctl is not on PATH")
	}
	if !strings.Contains(err.Error(), bootstrapengine.RuntimeCLI) {
		t.Errorf("error %q does not name the missing runtime", err)
	}
	if strings.Contains(err.Error(), "authentication failure") {
		t.Errorf("a missing binary was reported as an auth failure: %v", err)
	}
}

// runnerFunc adapts a function to bootstrapengine.CommandRunner.
type runnerFunc func(ctx context.Context, env map[string]string, name string, args ...string) (string, error)

func (f runnerFunc) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	return f(ctx, env, name, args...)
}

// The flag is gone outright rather than rejecting "docker" by value, so
// cobra's own unknown-flag error is the contract.
func TestBootstrapSiteRejectsBootstrapProviderFlag(t *testing.T) {
	cmd := newBootstrapCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"site", "-f", writeSiteSpec(t, validSiteSpec), "--bootstrap-provider", "docker"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --bootstrap-provider to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error %q does not report an unknown flag", err)
	}
}

// TestRunBootstrapSiteDefaultsImagesToTheLocalBuild confirms the bootstrap
// side-loads the local build's tags when neither image variable is set.
func TestRunBootstrapSiteDefaultsImagesToTheLocalBuild(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", "")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "")
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries:    func(context.Context, string, string) (int, error) { return 1, nil },
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err != nil {
		t.Fatalf("bootstrap with default images: %v", err)
	}
	for _, image := range []string{"nke-controller:latest", "cluster-api-provider-provisioned:latest"} {
		if !runner.sawArgs("image", "inspect", image) {
			t.Errorf("default image %s was not used; calls: %v", image, runner.calls)
		}
	}
}

// TestRunBootstrapSiteUsesTheImageFlags drives the two flags through the
// command: the images they name are the ones the bootstrap loads.
func TestRunBootstrapSiteUsesTheImageFlags(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", "env.example/nke-controller:env")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "env.example/provisioned:env")
	startFakeCWEAPI(t, true)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:                 writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:          runner,
		cweRunner:                newFakeCWERunner(t),
		kubeconfigOut:            filepath.Join(t.TempDir(), "site.kubeconfig"),
		lookPath:                 runtimeOnPath,
		dialContext:              refuseDial,
		untaintControlPlane:      skipUntaint,
		dumpSpireEntries:         func(context.Context, string, string) (int, error) { return 1, nil },
		encryptionKeyOut:         filepath.Join(t.TempDir(), "key.json"),
		controllerImage:          "nke-controller:flag",
		provisionedProviderImage: "cluster-api-provider-provisioned:flag",
	})
	if err != nil {
		t.Fatalf("bootstrap with image flags: %v", err)
	}
	for _, image := range []string{"nke-controller:flag", "cluster-api-provider-provisioned:flag"} {
		if !runner.sawArgs("image", "inspect", image) {
			t.Errorf("flag image %s was not used; calls: %v", image, runner.calls)
		}
	}
	for _, image := range []string{"env.example/nke-controller:env", "env.example/provisioned:env"} {
		if runner.sawArgs("image", "inspect", image) {
			t.Errorf("variable image %s was used although a flag named another; calls: %v", image, runner.calls)
		}
	}
}

// TestEnsureImageAvailableRefusesToPullAHostlessRef: a local build that is
// not in the image store fails with the build command and the flags and variables.
// Nothing is pulled, and the inspect failure stays in the message.
func TestEnsureImageAvailableRefusesToPullAHostlessRef(t *testing.T) {
	var pulled bool
	engine := bootstrapengine.New(runnerFunc(
		func(_ context.Context, _ map[string]string, _ string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "pull" {
				pulled = true
			}
			return "no such image: nke-controller:latest", errors.New("exit status 1")
		},
	), nil)

	err := ensureImageAvailable(context.Background(), engine,
		&bootstrapengine.Cluster{Name: "c"}, "nke-controller:latest", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error for a local build that is not in the store")
	}
	for _, want := range []string{"make -C cli images", "--nvke-controller-image", "NVKECTL_NVKE_CONTROLLER_IMAGE", "docker.io/<org>/<name>", "no such image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if pulled {
		t.Error("a reference without a registry was pulled")
	}
}

// TestRunBootstrapSiteRefusesMissingLocalImageBeforeCreate: with the default
// images and no local build, the command fails before any cluster is built.
func TestRunBootstrapSiteRefusesMissingLocalImageBeforeCreate(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", "")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "")

	inner := runnerFunc(func(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "image" && args[1] == "inspect" && !hasRegistryHost(args[2]) {
			return "no such image: " + args[2], errors.New("exit status 1")
		}
		return fakeBootstrapRunner{}.Run(ctx, env, name, args...)
	})
	runner := &recordingRunner{next: inner}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err == nil {
		t.Fatal("expected an error when the default local image is not in the store")
	}
	for _, want := range []string{"nke-controller:latest", "make -C cli images", "--nvke-controller-image", "NVKECTL_NVKE_CONTROLLER_IMAGE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("a missing local image should be caught before the cluster is built; calls: %v", runner.calls)
	}
}

// TestRunBootstrapSiteRefusesAMissingProfilesImageWithItsOwnRemedy: the profiles
// image is not one `make -C cli images` builds, so the error says which variable
// sets it and does not send the operator to that target.
func TestRunBootstrapSiteRefusesAMissingProfilesImageWithItsOwnRemedy(t *testing.T) {
	const profiles = "example-profiles:e2e"
	t.Setenv(controllerdeploy.ProfilesImageEnv, profiles)

	inner := runnerFunc(func(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "image" && args[1] == "inspect" && args[2] == profiles {
			return "no such image: " + args[2], errors.New("exit status 1")
		}
		return fakeBootstrapRunner{}.Run(ctx, env, name, args...)
	})
	runner := &recordingRunner{next: inner}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	})
	if err == nil {
		t.Fatal("expected an error when the profiles image is not in the store")
	}
	for _, want := range []string{profiles, controllerdeploy.ProfilesImageEnv, "other than latest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	for _, reject := range []string{"make -C cli images", "--controller-image", "NVKECTL_CONTROLLER_IMAGE"} {
		if strings.Contains(err.Error(), reject) {
			t.Errorf("error %q sends the operator to %q, which does not apply to the profiles image", err, reject)
		}
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("a missing profiles image should be caught before the cluster is built; calls: %v", runner.calls)
	}
}

// TestRunBootstrapSiteRefusesAProfilesImageTheDeployWouldRefuse: a latest or
// untagged profiles image is refused by the dry run, which stands nothing up, and
// by a real run before it builds a cluster. The deploy refuses it too, but only
// after the Kind cluster, the side-load and several installs.
func TestRunBootstrapSiteRefusesAProfilesImageTheDeployWouldRefuse(t *testing.T) {
	for name, image := range map[string]string{
		"an explicit latest tag":       "example-profiles:latest",
		"an untagged reference":        "example-profiles",
		"a registry image with latest": "registry.example.com/team/profiles:latest",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(controllerdeploy.ProfilesImageEnv, image)
			spec := writeSiteSpec(t, validSiteSpec)

			err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{siteSpec: spec, dryRun: true})
			if err == nil || !strings.Contains(err.Error(), "other than latest") || !strings.Contains(err.Error(), controllerdeploy.ProfilesImageEnv) {
				t.Errorf("dry run error = %v, want one naming %s and the latest tag", err, controllerdeploy.ProfilesImageEnv)
			}

			runner := &recordingRunner{next: fakeBootstrapRunner{}}
			err = runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
				siteSpec:         spec,
				bootstrapRunner:  runner,
				cweRunner:        newFakeCWERunner(t),
				lookPath:         runtimeOnPath,
				dialContext:      refuseDial,
				encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
			})
			if err == nil || !strings.Contains(err.Error(), "other than latest") {
				t.Errorf("real run error = %v, want the latest tag refused", err)
			}
			if len(runner.calls) != 0 {
				t.Errorf("a refused profiles image must stand nothing up; calls: %v", runner.calls)
			}
		})
	}

	t.Run("a tagged image passes the dry run", func(t *testing.T) {
		t.Setenv(controllerdeploy.ProfilesImageEnv, "example-profiles:e2e")
		if err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{siteSpec: writeSiteSpec(t, validSiteSpec), dryRun: true}); err != nil {
			t.Errorf("dry run with a tagged profiles image: %v", err)
		}
	})
}

// TestMakefileImageTagsMatchTheManifest: the tags `make -C cli images` builds
// are the defaults `bootstrap site` looks for. Both come from the manifest;
// the Makefile copies them by hand, so this pins the copy.
func TestMakefileImageTagsMatchTheManifest(t *testing.T) {
	mk, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("reading cli/Makefile: %v", err)
	}
	m := manifest.Load()
	for _, tc := range []struct{ variable, component, image string }{
		{"CONTROLLER_IMAGE", manifest.ComponentController, manifest.ImageController},
		{"CAPI_PROVISIONED_IMAGE", manifest.ComponentProvisioned, manifest.ImageManager},
	} {
		re := regexp.MustCompile(`(?m)^` + tc.variable + `\s*\?=\s*(\S+)\s*$`)
		match := re.FindSubmatch(mk)
		if match == nil {
			t.Fatalf("cli/Makefile does not set %s", tc.variable)
		}
		want := m.Components[tc.component].Images[tc.image].Ref
		if got := string(match[1]); got != want {
			t.Errorf("cli/Makefile %s = %q, manifest says %q", tc.variable, got, want)
		}
	}
}

// TestMakefileControllerImageFallsBackToTheOldName drives `make -n images`:
// CONTROLLER_IMAGE wins, CWE_IMAGE is used with a warning when
// CONTROLLER_IMAGE is unset or empty, from the environment or the command
// line, and neither gives the manifest default. #1399 removes CWE_IMAGE and
// this test with it.
func TestMakefileControllerImageFallsBackToTheOldName(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not on PATH")
	}
	const warning = "CWE_IMAGE is deprecated"
	for _, tc := range []struct {
		desc      string
		env, args []string
		wantTag   string
		wantWarn  bool
	}{
		{"neither", nil, nil, "nke-controller:latest", false},
		{"new name", nil, []string{"CONTROLLER_IMAGE=new:1"}, "new:1", false},
		{"old name from the environment", []string{"CWE_IMAGE=old:1"}, nil, "old:1", true},
		{"old name on the command line", nil, []string{"CWE_IMAGE=old:1"}, "old:1", true},
		{"both names", []string{"CWE_IMAGE=old:1"}, []string{"CONTROLLER_IMAGE=new:1"}, "new:1", false},
		{"old name with the new one empty on the command line", []string{"CWE_IMAGE=old:1"}, []string{"CONTROLLER_IMAGE="}, "old:1", true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			cmd := exec.Command(makeBin, append([]string{"-n", "-C", filepath.Join("..", ".."), "images"}, tc.args...)...)
			cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
				return strings.HasPrefix(kv, "CWE_IMAGE=") || strings.HasPrefix(kv, "CONTROLLER_IMAGE=")
			}), tc.env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make -n images: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "nerdctl build -t "+tc.wantTag+" ") {
				t.Errorf("controller image is not tagged %s:\n%s", tc.wantTag, out)
			}
			if got := strings.Contains(string(out), warning); got != tc.wantWarn {
				t.Errorf("deprecation warning printed = %v, want %v:\n%s", got, tc.wantWarn, out)
			}
		})
	}
}

// TestHasRegistryHost pins the rule: the first path component names a registry
// when it holds a dot or a colon, is localhost, or has an upper-case letter
// (the reference parser reads it as a domain). Docker Hub short names such as
// myorg/image:tag do not count; write docker.io/myorg/image:tag.
func TestHasRegistryHost(t *testing.T) {
	for ref, want := range map[string]bool{
		"nke-controller:latest":                   false,
		"cluster-api-provider-provisioned:latest": false,
		"nvcr.io/org/nvke-controller:edge":        true,
		"localhost:5000/nke-controller:dev":       true,
		"localhost/nke-controller:dev":            true,
		"ghcr.io/spiffe/spire-server@sha256:abcd": true,
		"library/postgres:18.6":                   false,
		"MyRegistry/nke-controller:1":             true,
	} {
		if got := hasRegistryHost(ref); got != want {
			t.Errorf("hasRegistryHost(%q) = %v, want %v", ref, got, want)
		}
	}
}

// recordingRunner wraps another CommandRunner and records the command name
// and args of every call, so a test can assert the bootstrap cluster was
// actually created and torn down without depending on bootstrapengine's own
// internals.
type recordingRunner struct {
	next  bootstrapengine.CommandRunner
	names []string
	calls [][]string
}

func (r *recordingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	r.names = append(r.names, name)
	r.calls = append(r.calls, args)
	return r.next.Run(ctx, env, name, args...)
}

func (r *recordingRunner) sawArgs(prefix ...string) bool {
	for _, call := range r.calls {
		if len(call) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if call[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func writeSiteSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write site spec: %v", err)
	}
	return path
}

// TestRunBootstrapSite_DiagnosticsOnlyOnRealFailures guards the distinction the
// dump depends on. A pre-create failure still tears the bootstrap cluster down,
// so its bundle has to be collected first. A successful run writes no bundle.
func TestRunBootstrapSite_DiagnosticsOnlyOnRealFailures(t *testing.T) {
	setBootstrapEnv(t)

	t.Run("a successful bootstrap writes nothing", func(t *testing.T) {
		startFakeCWEAPI(t, true)
		dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec:            writeSiteSpec(t, validSiteSpec),
			diagnosticsDir:      dir,
			bootstrapRunner:     fakeBootstrapRunner{},
			cweRunner:           newFakeCWERunner(t),
			kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
			lookPath:            runtimeOnPath,
			dialContext:         refuseDial,
			untaintControlPlane: skipUntaint,
			dumpSpireEntries: func(context.Context, string, string) (int, error) {
				return 0, nil
			},
			encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
		})
		if err != nil {
			t.Fatalf("runBootstrapSite returned error: %v", err)
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Error("a successful bootstrap should leave no diagnostics behind")
		}
	})

	t.Run("a successful bootstrap with collectDiagnostics writes a bundle", func(t *testing.T) {
		startFakeCWEAPI(t, true)
		dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec:            writeSiteSpec(t, validSiteSpec),
			diagnosticsDir:      dir,
			collectDiagnostics:  true,
			bootstrapRunner:     fakeBootstrapRunner{},
			cweRunner:           newFakeCWERunner(t),
			kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
			lookPath:            runtimeOnPath,
			dialContext:         refuseDial,
			untaintControlPlane: skipUntaint,
			dumpSpireEntries: func(context.Context, string, string) (int, error) {
				return 0, nil
			},
			encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
		})
		if err != nil {
			t.Fatalf("runBootstrapSite returned error: %v", err)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Errorf("no diagnostics written for a run that asked for them: %v", statErr)
		}
	})

	t.Run("a real failure writes a bundle", func(t *testing.T) {
		// Short, so the suite does not sit through the default five minutes
		// waiting for a controller that will never report healthy.
		t.Setenv("NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT", "1s")
		dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
		// The controller never reports healthy: the failure this exists for.
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec:            writeSiteSpec(t, validSiteSpec),
			diagnosticsDir:      dir,
			bootstrapRunner:     fakeBootstrapRunner{},
			cweRunner:           unhealthyCWERunner{},
			lookPath:            runtimeOnPath,
			dialContext:         refuseDial,
			untaintControlPlane: skipUntaint,
			encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		})
		if err == nil {
			t.Fatal("expected a real failure")
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Errorf("no diagnostics written for a real failure: %v", statErr)
		}
	})
}

// unhealthyCWERunner deploys fine but never reports healthy, which is the
// shape of the failure the diagnostics bundle is meant to explain.
type unhealthyCWERunner struct{ fakeCWERunner }

func (u unhealthyCWERunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "kubectl" && strings.Contains(strings.Join(args, " "), "readyz") {
		return `{"status":"starting"}`, nil
	}
	return u.fakeCWERunner.Run(ctx, name, args...)
}

type missingKubeconfigRunner struct{ fakeCWERunner }

func (r missingKubeconfigRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "kubectl" && strings.Contains(strings.Join(args, " "), "jsonpath={.data.value}") {
		return "Error from server (NotFound): secret not found", errors.New("exit status 1")
	}
	return r.fakeCWERunner.Run(ctx, name, args...)
}

// TestRunBootstrapSiteWaitsForEveryControlPlaneNode: a cluster reports
// Provisioned as soon as the one node it chose as the initial control-plane
// node has run kubeadm init. The other two join afterwards, through workflows
// that run in the bootstrap cluster this command deletes on its way out.
// Deleting it on the cluster condition alone strands whichever nodes had not
// finished, and a full re-bootstrap is the only way back.
//
// Both ends of the range are covered. One node joined is the reported bug: the
// command called a three-node cluster ready on the strength of the first node.
// Two joined is the straggler that is hardest to notice — the cluster is one
// node short of what was asked for, which is a quorum that works today and a
// control plane that cannot lose a node tomorrow.
func TestRunBootstrapSiteWaitsForEveryControlPlaneNode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		joined   int
		stranded []string
	}{
		{name: "only the initial node joined", joined: 1, stranded: []string{"node2", "node3"}},
		{name: "one node never joins", joined: 2, stranded: []string{"node3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBootstrapEnv(t)
			startFakeCWEAPIWithNodes(t, true, 3, tc.joined)

			runner := &recordingRunner{next: fakeBootstrapRunner{}}
			err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
				siteSpec:        writeSiteSpec(t, threeNodeSiteSpec),
				bootstrapRunner: runner,
				cweRunner:       newFakeCWERunner(t),
				// Longer than the 5ms the timeout-only tests above use. This
				// one asserts the error names how many nodes joined, so the
				// first read of the fake API has to land before the deadline.
				// At 5ms it did on Linux and did not on the macOS runner,
				// which reported "0 of 3". The deadline still ends the test.
				clusterTimeout:      500 * time.Millisecond,
				kubeconfigOut:       filepath.Join(t.TempDir(), "unused.kubeconfig"),
				lookPath:            runtimeOnPath,
				dialContext:         refuseDial,
				untaintControlPlane: skipUntaint,
				encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
			})
			if err == nil {
				t.Fatalf("bootstrap reported success with %d of 3 control plane nodes joined", tc.joined)
			}
			if runner.sawArgs("delete", "cluster") {
				t.Error("bootstrap kind cluster was torn down while a control plane node was still joining")
			}
			want := append([]string{
				fmt.Sprintf("%d of 3", tc.joined),
				"retained for recovery",
			}, tc.stranded...)
			for _, w := range want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			// The nodes that did finish are not stragglers, and naming them
			// would send the operator after the wrong host.
			for i := 1; i <= tc.joined; i++ {
				if node := fmt.Sprintf("node%d", i); strings.Contains(err.Error(), node) {
					t.Errorf("error %q lists %s, which had finished", err, node)
				}
			}
		})
	}
}

// TestRunBootstrapSiteTearsDownOnceEveryNodeHasJoined is the other half: the
// wait must end, and the ephemeral cluster must still be cleaned up.
func TestRunBootstrapSiteTearsDownOnceEveryNodeHasJoined(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPIWithNodes(t, true, 3, 3)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, threeNodeSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, nil
		},
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
	})
	if err != nil {
		t.Fatalf("runBootstrapSite returned error: %v", err)
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("runBootstrapSite did not tear down the bootstrap kind cluster; calls: %v", runner.calls)
	}
}

// fakeEtcdKey is the key material fakeCWERunner serves. Tests assert it
// reaches the file on disk and nothing else.
const fakeEtcdKey = "fake-etcd-key-material"

// missingEtcdKeyRunner fails the key Secret read with output that a leaking
// error would carry.
type missingEtcdKeyRunner struct{ fakeCWERunner }

const leakedEtcdKeyOutput = "OUTPUT-THAT-MUST-NOT-APPEAR"

func (r missingEtcdKeyRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "kubectl" && strings.Contains(strings.Join(args, " "), "get secret etcd-encryption-key-") {
		return leakedEtcdKeyOutput, errors.New("exit status 1")
	}
	return r.fakeCWERunner.Run(ctx, name, args...)
}

// recordingCWERunner records every controllerdeploy command so a test can assert one
// was never issued.
type recordingCWERunner struct {
	next  controllerdeploy.CommandRunner
	calls []string
}

func (r *recordingCWERunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return r.next.Run(ctx, name, args...)
}

func (r *recordingCWERunner) saw(fragment string) bool {
	for _, call := range r.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

// TestRunBootstrapSiteSavesTheEtcdEncryptionKey: the key the controller generated ends
// up on disk, as a Secret the operator can hand to a later controller, before
// the bootstrap cluster holding the controller's copy is deleted.
func TestRunBootstrapSiteSavesTheEtcdEncryptionKey(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	keyOut := filepath.Join(t.TempDir(), "site-key.json")
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "site.kubeconfig"),
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite: %v", err)
	}

	info, err := os.Stat(keyOut)
	if err != nil {
		t.Fatalf("the key file was not written: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key file mode = %o, want 0600", mode)
	}
	body, err := os.ReadFile(keyOut)
	if err != nil {
		t.Fatal(err)
	}
	var secret struct {
		Metadata struct {
			Name        string            `json:"name"`
			Namespace   *string           `json:"namespace"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal(body, &secret); err != nil {
		t.Fatalf("key file is not a Secret manifest: %v", err)
	}
	if !strings.HasPrefix(secret.Metadata.Name, "etcd-encryption-key-") {
		t.Errorf("metadata.name = %q, want the controller's etcd-encryption-key-<uuid>", secret.Metadata.Name)
	}
	if secret.Metadata.Namespace != nil {
		t.Errorf("metadata.namespace = %q, want none", *secret.Metadata.Namespace)
	}
	if secret.Metadata.Annotations["nvke.nvidia.com/cluster-name"] != "nvke-site" {
		t.Errorf("cluster-name annotation = %q, want %q", secret.Metadata.Annotations["nvke.nvidia.com/cluster-name"], "nvke-site")
	}
	if string(secret.Data["active"]) != fakeEtcdKey {
		t.Errorf("data.active is not the key the controller served")
	}
	if !runner.sawArgs("delete", "cluster") {
		t.Errorf("a successful bootstrap should still tear down the bootstrap kind cluster; calls: %v", runner.calls)
	}
}

// TestRunBootstrapSiteRetainsClusterWhenKeyCaptureFails: the cluster is up
// and encrypted, and the only copy of its key the operator does not have is in
// the bootstrap cluster. That cluster is kept, the message says how to get the
// key out, and it does not end — as the generic message does — by telling the
// operator to delete it.
func TestRunBootstrapSiteRetainsClusterWhenKeyCaptureFails(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	keyOut := filepath.Join(t.TempDir(), "key.json")
	kubeconfigOut := filepath.Join(t.TempDir(), "site.kubeconfig")
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           missingEtcdKeyRunner{fakeCWERunner: newFakeCWERunner(t)},
		kubeconfigOut:       kubeconfigOut,
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
	})
	if err == nil {
		t.Fatal("expected an error when the key Secret cannot be read")
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted while it held the only reachable copy of the key")
	}
	// The recovery command writes what the tool would have: to the path the
	// operator chose, owner-readable only, the Secret the controller holds.
	for _, want := range []string{
		"was not saved to " + keyOut,
		"kubeconfig is at " + kubeconfigOut,
		"do not delete it",
		"umask 077 && kubectl --kubeconfig",
		"-n nvke-controller get secret etcd-encryption-key-",
		"-o json > " + keyOut,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	for _, unwanted := range []string{leakedEtcdKeyOutput, "kind delete cluster"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error %q contains %q", err, unwanted)
		}
	}
	if _, statErr := os.Stat(kubeconfigOut); statErr != nil {
		t.Errorf("the kubeconfig was not written before the key capture failed: %v", statErr)
	}
}

// TestRequireNoKeyFile: only a path that does not exist is free. Anything that
// cannot be checked would fail the write at the very end of a bootstrap.
func TestRequireNoKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := requireNoKeyFile(filepath.Join(dir, "absent.json")); err != nil {
		t.Errorf("absent path: err = %v, want nil", err)
	}

	existing := filepath.Join(dir, "existing.json")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireNoKeyFile(existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing path: err = %v, want \"already exists\"", err)
	}

	// A regular file where a directory is needed: Lstat fails with ENOTDIR,
	// which is not "free".
	if err := requireNoKeyFile(filepath.Join(existing, "key.json")); err == nil || !strings.Contains(err.Error(), "checking") {
		t.Errorf("unwritable path: err = %v, want a check failure", err)
	}
}

// TestRunBootstrapSiteSkipsKeyCaptureWhenEncryptionIsOff: with encryption
// off there is no Secret to read, and a read that waited for one would fail a
// healthy bootstrap.
func TestRunBootstrapSiteSkipsKeyCaptureWhenEncryptionIsOff(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	keyOut := filepath.Join(t.TempDir(), "key.json")
	cwe := &recordingCWERunner{next: newFakeCWERunner(t)}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, strings.Replace(validSiteSpec, "  k8sVersion:", "  etcdEncryption: false\n  k8sVersion:", 1)),
		bootstrapRunner:     &recordingRunner{next: fakeBootstrapRunner{}},
		cweRunner:           cwe,
		kubeconfigOut:       filepath.Join(t.TempDir(), "site.kubeconfig"),
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite: %v", err)
	}
	if cwe.saw("etcd-encryption-key-") {
		t.Errorf("the key Secret was read for an unencrypted cluster; calls: %v", cwe.calls)
	}
	if _, statErr := os.Stat(keyOut); statErr == nil {
		t.Error("a key file was written for an unencrypted cluster")
	}
}

// TestRunBootstrapSiteRefusesAnExistingKeyFile: the file is never overwritten,
// and a run that would fail on that at the very end is refused at the start.
func TestRunBootstrapSiteRefusesAnExistingKeyFile(t *testing.T) {
	setBootstrapEnv(t)

	keyOut := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(keyOut, []byte("earlier"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		encryptionKeyOut:    keyOut,
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
	})
	if err == nil {
		t.Fatal("expected an error when the key file already exists")
	}
	for _, want := range []string{keyOut, "already exists", "delete the file and re-run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("the bootstrap kind cluster was created despite an existing key file; calls: %v", runner.calls)
	}
}

// skipUntaint keeps tests that are not about the untaint from building a
// client out of the fake kubeconfig.
func skipUntaint(context.Context, string, *slog.Logger) ([]string, error) { return nil, nil }

// refuseDial stands in for a site spec host with nothing on the API port, so
// the pre-create probe passes without dialling anything real.
func refuseDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("connection refused")
}

// TestRunBootstrapSiteRefusesHostsServingAControlPlane: a host that already
// answers on the API port is not provisioned over. The message names the host
// and the way out, and nothing has been built by the time it is printed.
func TestRunBootstrapSiteRefusesHostsServingAControlPlane(t *testing.T) {
	setBootstrapEnv(t)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:         writeSiteSpec(t, threeNodeSiteSpec),
		bootstrapRunner:  runner,
		cweRunner:        newFakeCWERunner(t),
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
		lookPath:         runtimeOnPath,
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			if address != "10.0.0.2:6443" {
				return nil, errors.New("connection refused")
			}
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	})
	if err == nil {
		t.Fatal("expected an error when a site spec host already serves a control plane")
	}
	for _, want := range []string{"node2 (10.0.0.2)", "6443", "kubeadm reset -f"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "node1") || strings.Contains(err.Error(), "node3") {
		t.Errorf("error %q names a host that refused the connection", err)
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("the bootstrap kind cluster was created despite a host already serving a control plane; calls: %v", runner.calls)
	}
}

// leftoverKindRunner reports the bootstrap cluster as already present, as a
// run that kept it — or failed and retained it — leaves things.
type leftoverKindRunner struct{ fakeBootstrapRunner }

func (r leftoverKindRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	if name == "kind" && len(args) >= 2 && args[0] == "get" && args[1] == "clusters" {
		return "nvkectl-bootstrap\n", nil
	}
	return r.fakeBootstrapRunner.Run(ctx, env, name, args...)
}

// nicoSiteSpecFile writes examples/sitespec-nico.yaml with the files it names
// beside it, and points NVKECTL_NICO_CREDENTIALS at a token file, so the
// bootstrap checks pass.
func nicoSiteSpecFile(t *testing.T) string {
	t.Helper()
	example, err := os.ReadFile("../../examples/sitespec-nico.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := writeSiteSpec(t, string(example))
	dir := filepath.Dir(path)
	for _, name := range []string{"nico.token"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("example-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(sitespec.NicoCredentialsEnv, filepath.Join(dir, "nico.token"))
	t.Setenv(stackconfig.EnvNodeUser, "example-user")
	t.Setenv(stackconfig.EnvNodePassword, "example-password")
	if err := os.Mkdir(filepath.Join(dir, "bootstrap-releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	const releases = "apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n" +
		"---\napiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-ubuntu-24.04-amd64\n" +
		"spec:\n  machineImageRef:\n    name: img\n"
	if err := os.WriteFile(filepath.Join(dir, "bootstrap-releases", "r.yaml"), []byte(releases), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunBootstrapSiteDryRunReportsBootstrapProblemsTogether: the dry run is the
// complete pre-flight, so a missing SPIRE address does not hide the NICo token
// the bootstrap host also lacks.
func TestRunBootstrapSiteDryRunReportsBootstrapProblemsTogether(t *testing.T) {
	example, err := os.ReadFile("../../examples/sitespec-nico.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(example), `  externalAddress: "192.0.2.10"`, "", 1)
	if text == string(example) {
		t.Fatal("the example no longer sets spire.externalAddress; update this test")
	}
	t.Setenv(sitespec.NicoCredentialsEnv, "")

	err = runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{siteSpec: writeSiteSpec(t, text), dryRun: true})
	if err == nil {
		t.Fatal("the dry run passed a spec with no SPIRE address and no NICo token")
	}
	for _, want := range []string{"spire.externalAddress is required", sitespec.NicoCredentialsEnv + " must name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestRunBootstrapSiteNicoLeftoverClusterSaysToReleaseInstancesFirst: with nico
// the leftover-cluster refusal is the only re-run guard, and the "delete it and
// re-run" it ends with would orphan the NICo instances an earlier run
// allocated. The refusal names the release step before the delete.
func TestRunBootstrapSiteNicoLeftoverClusterSaysToReleaseInstancesFirst(t *testing.T) {
	setBootstrapEnv(t)

	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            nicoSiteSpecFile(t),
		bootstrapRunner:     &recordingRunner{next: leftoverKindRunner{}},
		cweRunner:           newFakeCWERunner(t),
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
	})
	if err == nil {
		t.Fatal("expected an error while an earlier bootstrap cluster is still running")
	}
	msg := err.Error()
	for _, want := range []string{"`nvkectl delete cluster nvke-site`", "nvkectl config set-site --name nvkectl-bootstrap --url http://127.0.0.1:30080", "no longer lists it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Index(msg, "nvkectl delete cluster") > strings.Index(msg, "kind delete cluster") {
		t.Errorf("error %q tells the operator to delete the Kind cluster before it releases the instances", msg)
	}
	// A leftover may hold a working site cluster, for which "delete it and
	// re-run" allocates a second set of machines behind the same VIP.
	for _, want := range []string{"succeeded, or failed after every control-plane node joined", "delete only this cluster", "do not re-run", "second control plane"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// TestRunBootstrapSiteNicoSpireDumpFailureIsAdvisedAsAJoinedCluster: the SPIRE
// export runs after the site cluster is built. A failure there leaves a working
// site, so the error says to delete only the bootstrap cluster, not to release
// the NICo instances (which would delete the site and its machines).
func TestRunBootstrapSiteNicoSpireDumpFailureIsAdvisedAsAJoinedCluster(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPIWithNodes(t, true, 3, 3)

	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            nicoSiteSpecFile(t),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			return 0, errors.New("dump failed")
		},
	})
	if err == nil {
		t.Fatal("expected the SPIRE dump failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"writing SPIRE bootstrap resources", "the site cluster is up and is not to be deleted",
		"delete only the bootstrap cluster with `KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	for _, reject := range []string{"nvkectl delete cluster", "release the NICo instances", "set-site"} {
		if strings.Contains(msg, reject) {
			t.Errorf("error %q contains %q, which would delete a working site cluster", msg, reject)
		}
	}
	if runner.sawArgs("delete", "cluster") {
		t.Error("bootstrap cluster was deleted after the SPIRE copy failed")
	}
}

// TestRunBootstrapSiteNicoRefusesWhenTheVIPAlreadyAnswers: the one address a
// nico site's control plane is known by before it exists. When it answers, a
// site for this spec is running, and a second bootstrap would allocate another
// set of NICo machines behind the same VIP. Nothing is built before the refusal.
func TestRunBootstrapSiteNicoRefusesWhenTheVIPAlreadyAnswers(t *testing.T) {
	setBootstrapEnv(t)

	var dialed []string
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:         nicoSiteSpecFile(t),
		bootstrapRunner:  runner,
		cweRunner:        newFakeCWERunner(t),
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
		lookPath:         runtimeOnPath,
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			if address != "10.0.0.100:6443" {
				return nil, errors.New("connection refused")
			}
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	})
	if err == nil {
		t.Fatal("expected an error when the control-plane VIP already answers")
	}
	for _, want := range []string{"10.0.0.100", "6443", "do not re-run", "another set of NICo machines"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if !slices.Equal(dialed, []string{"10.0.0.100:6443"}) {
		t.Errorf("dialed %v, want only the VIP: a nico spec lists no hosts", dialed)
	}
	if runner.sawArgs("create", "cluster") {
		t.Errorf("the bootstrap kind cluster was created although the VIP already answers; calls: %v", runner.calls)
	}
}

// TestRequireNoSiteAtVIPSkipsAnEmptyVIP: a one-node nico spec may have no VIP,
// and then there is nothing to dial.
func TestRequireNoSiteAtVIPSkipsAnEmptyVIP(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed with no VIP configured")
		return nil, errors.New("unexpected dial")
	}
	if err := requireNoSiteAtVIP(context.Background(), "", dial); err != nil {
		t.Fatalf("requireNoSiteAtVIP with no VIP: %v", err)
	}
}

// TestCreateSiteCluster_KeepsKindAfterAnUnconfirmedNicoCreate: with nico, a
// create the controller may have committed may be allocating NICo instances,
// so the bootstrap cluster is kept. A refusal created nothing, and the
// provisioned provider allocates nothing, so both leave it to be deleted.
func TestCreateSiteCluster_KeepsKindAfterAnUnconfirmedNicoCreate(t *testing.T) {
	nico, err := sitespec.Load(nicoSiteSpecFile(t))
	if err != nil {
		t.Fatal(err)
	}
	provisioned, err := sitespec.Load(writeSiteSpec(t, validSiteSpec))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		inv      *sitespec.SiteSpec
		status   int
		wantKept bool
	}{
		{name: "nico, a 5xx", inv: nico, status: http.StatusInternalServerError, wantKept: true},
		{name: "nico, a 4xx refusal", inv: nico, status: http.StatusBadRequest},
		{name: "provisioned, a 5xx", inv: provisioned, status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			port := srv.Listener.Addr().(*net.TCPAddr).Port

			kept, err := createSiteCluster(context.Background(), nil, tt.inv, &bootstrapSiteOptions{}, int32(port), slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("expected createSiteCluster to fail")
			}
			if kept != tt.wantKept {
				t.Errorf("createSiteCluster kept the bootstrap cluster = %v, want %v (err %v)", kept, tt.wantKept, err)
			}
		})
	}
}

// TestRetainedClusterError: the advice after a failure past CreateCluster. With
// nico it leads with releasing the instances; with provisioned it is unchanged.
func TestRetainedClusterError(t *testing.T) {
	cluster := &bootstrapengine.Cluster{Name: "nvkectl-bootstrap", KubeconfigPath: "/tmp/kc"}
	cause := errors.New("workflow failed")
	nico := &sitespec.SiteSpec{ClusterName: "nvke-site", Infra: sitespec.Infra{Provider: sitespec.ProviderNico}}
	provisioned := &sitespec.SiteSpec{ClusterName: "nvke-site"}

	t.Run("provisioned", func(t *testing.T) {
		msg := retainedClusterError(cause, provisioned, cluster, 30080).Error()
		if strings.Contains(msg, "NICo") || !strings.Contains(msg, "delete it after recovery with `KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap`") {
			t.Errorf("provisioned advice changed: %q", msg)
		}
	})
	t.Run("nico", func(t *testing.T) {
		msg := retainedClusterError(cause, nico, cluster, 30080).Error()
		for _, want := range []string{
			"release the NICo instances", "leaves them allocated and unmanaged", "tenant login",
			"`nvkectl config set-site --name nvkectl-bootstrap --url http://127.0.0.1:30080 --kubeconfig /tmp/kc`",
			"`nvkectl delete cluster nvke-site`", "nvkectl config use-site",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("nico advice %q does not contain %q", msg, want)
			}
		}
		if strings.Index(msg, "nvkectl delete cluster") > strings.Index(msg, "kind delete cluster") {
			t.Errorf("advice %q deletes the Kind cluster before it releases the instances", msg)
		}
	})
	// The create may have been committed without its answer arriving: the advice
	// says to check for the cluster before releasing it, then delete Kind.
	t.Run("nico with the create unconfirmed", func(t *testing.T) {
		unconfirmed := fmt.Errorf("sitecluster: creating cluster %q: %w: %w", "nvke-site", sitecluster.ErrCreateUnconfirmed, cause)
		msg := retainedClusterError(unconfirmed, nico, cluster, 30080).Error()
		for _, want := range []string{
			"no answer from the controller confirmed or refused the create",
			"`nvkectl config set-site --name nvkectl-bootstrap --url http://127.0.0.1:30080 --kubeconfig /tmp/kc`",
			"run `nvkectl get clusters`; if it lists nvke-site", "leaves them allocated and unmanaged",
			"`nvkectl delete cluster nvke-site`", "nvkectl config use-site",
			"then delete the bootstrap cluster with `KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap`",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("unconfirmed-create advice %q does not contain %q", msg, want)
			}
		}
		if strings.Index(msg, "nvkectl get clusters") > strings.Index(msg, "nvkectl delete cluster") {
			t.Errorf("advice %q deletes the site cluster before checking it exists", msg)
		}
	})
	// After every node joined the site cluster is working: following the release
	// steps would delete it and its NICo machines, so they are not given.
	t.Run("nico with the key unsaved", func(t *testing.T) {
		capture := &keyCaptureError{clusterName: "nvke-site", keyPath: "k.json", kubeconfigPath: "/tmp/kc", recover: "save-it", err: cause}
		msg := retainedClusterError(capture, nico, cluster, 30080).Error()
		for _, reject := range []string{"nvkectl delete cluster", "release the NICo instances", "set-site"} {
			if strings.Contains(msg, reject) {
				t.Errorf("key-unsaved advice %q contains %q, which would delete a working site cluster", msg, reject)
			}
		}
		if !strings.Contains(msg, "once the key is saved, delete only the bootstrap cluster with `KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap`") {
			t.Errorf("key-unsaved advice does not say to delete only the bootstrap cluster: %q", msg)
		}
	})
	t.Run("nico with the kubeconfig unwritten", func(t *testing.T) {
		msg := retainedClusterError(&clusterJoinedError{cause}, nico, cluster, 30080).Error()
		for _, reject := range []string{"nvkectl delete cluster", "release the NICo instances", "set-site"} {
			if strings.Contains(msg, reject) {
				t.Errorf("post-join advice %q contains %q, which would delete a working site cluster", msg, reject)
			}
		}
		for _, want := range []string{"site cluster is up", "delete only the bootstrap cluster", "kind delete cluster --name nvkectl-bootstrap"} {
			if !strings.Contains(msg, want) {
				t.Errorf("post-join advice %q does not contain %q", msg, want)
			}
		}
	})
}

// TestRunBootstrapSiteRefusesWhileAnEarlierBootstrapClusterRuns: the cluster
// left by an earlier run may hold the key that run did not save. The refusal
// says how to get it out before it says how to delete the cluster, and does
// not delete or reuse anything itself.
func TestRunBootstrapSiteRefusesWhileAnEarlierBootstrapClusterRuns(t *testing.T) {
	setBootstrapEnv(t)

	runner := &recordingRunner{next: leftoverKindRunner{}}
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     runner,
		cweRunner:           newFakeCWERunner(t),
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
	})
	if err == nil {
		t.Fatal("expected an error while an earlier bootstrap cluster is still running")
	}
	msg := err.Error()
	for _, want := range []string{
		"still running",
		"kind get kubeconfig --name nvkectl-bootstrap",
		"etcd-encryption-key-",
		"get secrets -A | grep etcd-encryption-key-",
		"kind delete cluster --name nvkectl-bootstrap",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Index(msg, "get kubeconfig") > strings.Index(msg, "delete cluster") {
		t.Errorf("error %q tells the operator to delete before it tells them to save the key", msg)
	}
	if runner.sawArgs("create", "cluster") || runner.sawArgs("delete", "cluster") {
		t.Errorf("the check must neither create nor delete a bootstrap cluster; calls: %v", runner.calls)
	}
}

// TestRunBootstrapSiteRemovesTheControlPlaneTaint: the untaint runs by
// default, against the kubeconfig the command just wrote.
func TestRunBootstrapSiteRemovesTheControlPlaneTaint(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nvke-site.kubeconfig")
	untaintedWith := ""
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:         writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:  &recordingRunner{next: fakeBootstrapRunner{}},
		cweRunner:        newFakeCWERunner(t),
		kubeconfigOut:    kubeconfigOut,
		encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
		lookPath:         runtimeOnPath,
		dialContext:      refuseDial,
		dumpSpireEntries: func(context.Context, string, string) (int, error) { return 0, nil },
		untaintControlPlane: func(_ context.Context, kubeconfigPath string, _ *slog.Logger) ([]string, error) {
			untaintedWith = kubeconfigPath
			return []string{"node1"}, nil
		},
	})
	if err != nil {
		t.Fatalf("runBootstrapSite: %v", err)
	}
	if untaintedWith != kubeconfigOut {
		t.Errorf("untaint ran against %q, want the written kubeconfig %q", untaintedWith, kubeconfigOut)
	}
}

// TestRunBootstrapSiteReportsAnUntaintFailure: the untaint is the last step,
// so everything else is on disk and already reported when it fails, and the
// bootstrap cluster is neither dumped nor kept.
func TestRunBootstrapSiteReportsAnUntaintFailure(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	kubeconfigOut := filepath.Join(t.TempDir(), "nvke-site.kubeconfig")
	keyOut := filepath.Join(t.TempDir(), "key.json")
	spireEntriesDumped := false
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	cwe := &recordingCWERunner{next: newFakeCWERunner(t)}
	var out bytes.Buffer
	err := runBootstrapSite(context.Background(), &out, &bootstrapSiteOptions{
		siteSpec:         writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:  runner,
		cweRunner:        cwe,
		kubeconfigOut:    kubeconfigOut,
		encryptionKeyOut: keyOut,
		lookPath:         runtimeOnPath,
		dialContext:      refuseDial,
		dumpSpireEntries: func(context.Context, string, string) (int, error) {
			spireEntriesDumped = true
			return 1, nil
		},
		untaintControlPlane: func(context.Context, string, *slog.Logger) ([]string, error) {
			return nil, errors.New("nodes is forbidden")
		},
	})
	if err == nil {
		t.Fatal("runBootstrapSite returned nil, want the untaint failure")
	}
	if !spireEntriesDumped {
		t.Error("the SPIRE export did not run; the untaint must come after it")
	}
	if runner.sawArgs("delete", "cluster") {
		t.Errorf("the bootstrap kind cluster was deleted after the untaint failed; calls: %v", runner.calls)
	}
	if cwe.saw("all,events") {
		t.Errorf("bootstrap cluster diagnostics were collected for a failure in the site cluster; calls: %v", cwe.calls)
	}
	for _, want := range []string{"nodes is forbidden", "kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule-", kubeconfigOut} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, path := range []string{kubeconfigOut, keyOut} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("%s is missing after the untaint failure: %v", path, statErr)
		}
	}
	if !strings.Contains(out.String(), keyOut) {
		t.Errorf("the etcd key location was not reported before the untaint; output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "is ready") {
		t.Errorf("the cluster was reported ready although the untaint failed; output:\n%s", out.String())
	}
}

// TestMakeControlPlaneSchedulableBuildsTheClientFromTheKubeconfig covers the
// production path, with a kubeconfig that cannot be loaded.
func TestMakeControlPlaneSchedulableBuildsTheClientFromTheKubeconfig(t *testing.T) {
	inv, err := sitespec.Load(writeSiteSpec(t, validSiteSpec))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.kubeconfig")

	err = makeControlPlaneSchedulable(context.Background(), inv, missing, &bootstrapSiteOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("makeControlPlaneSchedulable returned nil for a kubeconfig that does not exist")
	}
	for _, want := range []string{missing, "kubectl taint nodes --all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestCWEDeployConfigImagePrecedence: a flag wins over its variable, and a
// variable wins over the manifest default.
func TestCWEDeployConfigImagePrecedence(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", "env.example/nke-controller:env")
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "env.example/provisioned:env")

	cfg, err := cweDeployConfig(&sitespec.SiteSpec{}, "c", false, "flag.example/nke-controller:flag", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CWEImage != "flag.example/nke-controller:flag" {
		t.Errorf("CWEImage = %q, want the flag", cfg.CWEImage)
	}
	if cfg.ProvisionedProviderImage != "env.example/provisioned:env" {
		t.Errorf("ProvisionedProviderImage = %q, want the variable", cfg.ProvisionedProviderImage)
	}

	cfg, err = cweDeployConfig(&sitespec.SiteSpec{}, "c", false, "", "flag.example/provisioned:flag")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CWEImage != "env.example/nke-controller:env" {
		t.Errorf("CWEImage = %q, want the variable", cfg.CWEImage)
	}
	if cfg.ProvisionedProviderImage != "flag.example/provisioned:flag" {
		t.Errorf("ProvisionedProviderImage = %q, want the flag", cfg.ProvisionedProviderImage)
	}
}

// TestControllerImageOrder pins the whole order for the
// controller image: the flag, then NVKECTL_NVKE_CONTROLLER_IMAGE, then the
// manifest default. The earlier NVKECTL_CONTROLLER_IMAGE is never read.
func TestControllerImageOrder(t *testing.T) {
	for _, tc := range []struct {
		desc, flag, newV, oldV, want string
	}{
		{"flag over both settings", "flag.example/c:flag", "new.example/c:new", "old.example/c:old", "flag.example/c:flag"},
		{"new setting over old", "", "new.example/c:new", "old.example/c:old", "new.example/c:new"},
		{"old setting alone", "", "", "old.example/c:old", "nke-controller:latest"},
		{"neither", "", "", "", "nke-controller:latest"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("NVKECTL_NVKE_CONTROLLER_IMAGE", tc.newV)
			t.Setenv("NVKECTL_CONTROLLER_IMAGE", tc.oldV)
			cfg, err := cweDeployConfig(&sitespec.SiteSpec{}, "c", false, tc.flag, "")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CWEImage != tc.want {
				t.Errorf("CWEImage = %q, want %q", cfg.CWEImage, tc.want)
			}
		})
	}
}

// TestCapnicoAccess: the bootstrap CAPNICo's endpoint and organization come from
// the site spec's infra.nico, its token from the file NVKECTL_NICO_CREDENTIALS
// names, and only for the nico provider.
func TestCapnicoAccess(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	nicoToken := write("nico.token", "example-nico-token\n")
	empty := write("empty.token", " \n")
	structured := write("client.yaml", "tokenURL: https://idp.example/token\nclientID: id\nclientSecret: secret\n")

	nico := &sitespec.SiteSpec{
		Infra: sitespec.Infra{
			Provider: sitespec.ProviderNico,
			Nico:     &sitespec.NicoInfra{OrgID: "example-org", Remote: &sitespec.NicoRemote{Endpoint: "https://nico.example"}},
		},
	}

	t.Run("provisioned needs nothing", func(t *testing.T) {
		got, err := capnicoAccess(&sitespec.SiteSpec{Infra: sitespec.Infra{Provider: sitespec.ProviderProvisioned}})
		if err != nil || got != (controllerdeploy.CAPNICoAccess{}) {
			t.Fatalf("capnicoAccess = %+v, %v; want the zero value", got, err)
		}
	})
	t.Run("nico reads the token file", func(t *testing.T) {
		t.Setenv(sitespec.NicoCredentialsEnv, nicoToken)
		got, err := capnicoAccess(nico)
		if err != nil {
			t.Fatalf("capnicoAccess: %v", err)
		}
		want := controllerdeploy.CAPNICoAccess{
			Endpoint: "https://nico.example", OrgID: "example-org", APIName: sitespec.DefaultNicoAPIName, Token: "example-nico-token",
		}
		if got != want {
			t.Errorf("capnicoAccess = %+v, want %+v", got, want)
		}
	})
	t.Run("nico takes the site spec's API name", func(t *testing.T) {
		t.Setenv(sitespec.NicoCredentialsEnv, nicoToken)
		carbide := *nico
		carbideNico := *nico.Infra.Nico
		carbideNico.Remote = &sitespec.NicoRemote{Endpoint: "https://nico.example", APIName: "carbide"}
		carbide.Infra.Nico = &carbideNico
		got, err := capnicoAccess(&carbide)
		if err != nil || got.APIName != "carbide" {
			t.Fatalf("capnicoAccess = %+v, %v; want APIName carbide", got, err)
		}
	})
	for name, tt := range map[string]struct {
		env     string
		inv     *sitespec.SiteSpec
		wantErr string
	}{
		"credentials variable unset": {"", nico, sitespec.NicoCredentialsEnv + " must name"},
		"missing NICo token file":    {filepath.Join(dir, "absent"), nico, sitespec.NicoCredentialsEnv},
		"empty NICo token file":      {empty, nico, "is empty"},
		"client credentials refused": {structured, nico, "client-credentials form is not supported"},
		"no NICo block in the spec":  {nicoToken, &sitespec.SiteSpec{Infra: sitespec.Infra{Provider: sitespec.ProviderNico}}, "infra.nico.remote"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(sitespec.NicoCredentialsEnv, tt.env)
			if _, err := capnicoAccess(tt.inv); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestCapnicoReleaseImage: a release's capnico-controller artifact becomes the
// bootstrap CAPNICo image as repository@digest, for the architecture asked, and
// only with the nico provider.
func TestCapnicoReleaseImage(t *testing.T) {
	const release = "../../../deploy/testdata/nvkr-release/valid.json"
	nico := &sitespec.SiteSpec{Infra: sitespec.Infra{Provider: sitespec.ProviderNico}}
	provisioned := &sitespec.SiteSpec{Infra: sitespec.Infra{Provider: sitespec.ProviderProvisioned}}

	tests := []struct {
		name    string
		inv     *sitespec.SiteSpec
		release string
		arch    string
		want    string
		wantErr string
	}{
		{"no release leaves the manifest's image", nico, "", "linux/amd64", "", ""},
		{"amd64", nico, release, "linux/amd64", "ghcr.io/example/recipes/capnico-controller@sha256:" + strings.Repeat("d", 64), ""},
		{"arm64", nico, release, "linux/arm64", "ghcr.io/example/recipes/capnico-controller-arm64@sha256:" + strings.Repeat("e", 64), ""},
		{"provisioned refuses a release", provisioned, release, "linux/amd64", "", "only applies with infra.provider nico"},
		{"missing release file", nico, filepath.Join(t.TempDir(), "absent.json"), "linux/amd64", "", "absent.json"},
		{"unknown architecture", nico, release, "linux/riscv64", "", "arch linux/riscv64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := capnicoReleaseImage(tt.inv, tt.release, tt.arch)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("capnicoReleaseImage = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// TestCapnicoReleaseImage_DefaultsToTheHostArchitecture: an empty architecture,
// the flag's default, picks the artifact for the machine the bootstrap runs on.
func TestCapnicoReleaseImage_DefaultsToTheHostArchitecture(t *testing.T) {
	const release = "../../../deploy/testdata/nvkr-release/valid.json"
	nico := &sitespec.SiteSpec{Infra: sitespec.Infra{Provider: sitespec.ProviderNico}}

	got, err := capnicoReleaseImage(nico, release, "")
	if err != nil {
		t.Fatalf("capnicoReleaseImage with no architecture: %v", err)
	}
	want, err := capnicoReleaseImage(nico, release, "linux/"+goruntime.GOARCH)
	if err != nil {
		t.Fatalf("capnicoReleaseImage for the host architecture: %v", err)
	}
	if got != want {
		t.Fatalf("an empty architecture gave %q, want the host's %q", got, want)
	}
}

// TestRunBootstrapSiteWarnsAboutOldSettingNames: an old NKECTL_CWE_* name set
// without its replacement is logged as a warning naming the replacement, before
// anything else runs; a setting under its new name is not.
func TestRunBootstrapSiteWarnsAboutOldSettingNames(t *testing.T) {
	t.Setenv("NVKECTL_CAPI_PROVISIONED_IMAGE", "")
	t.Setenv("NKECTL_CAPI_PROVISIONED_IMAGE", "old.example/p:old")
	t.Setenv("NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT", "2m")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A missing site spec stops the command right after the warnings.
	_ = runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec: filepath.Join(t.TempDir(), "missing.yaml"),
	})

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") ||
		!strings.Contains(logged, "NKECTL_CAPI_PROVISIONED_IMAGE is deprecated and will be removed in a future release; set NVKECTL_CAPI_PROVISIONED_IMAGE instead") {
		t.Errorf("no deprecation warning for NKECTL_CAPI_PROVISIONED_IMAGE in:\n%s", logged)
	}
	if strings.Contains(logged, "HELM_TIMEOUT") {
		t.Errorf("warned about a setting given under its new name:\n%s", logged)
	}
}

// TestRunBootstrapSiteIgnoresEarlierControllerNames: the earlier names of the
// NVKE controller settings (NKECTL_CWE_*, NKECTL_CONTROLLER_* and
// NVKECTL_CONTROLLER_*) are not read, so they neither stop bootstrap site nor
// draw a warning.
func TestRunBootstrapSiteIgnoresEarlierControllerNames(t *testing.T) {
	for _, name := range []string{"NKECTL_CWE_IMAGE", "NKECTL_CONTROLLER_PORT", "NVKECTL_CONTROLLER_IMAGE"} {
		t.Setenv(name, "old.example/c:pinned")
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A missing site spec is the first thing that stops the command.
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec: filepath.Join(t.TempDir(), "missing.yaml"),
	})
	if err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Fatalf("runBootstrapSite error = %v, want the missing site spec", err)
	}
	if logged := buf.String(); strings.Contains(logged, "level=WARN") {
		t.Errorf("warned about a name nvkectl does not read:\n%s", logged)
	}
}

// TestBootstrapSite_ControllerImageFlagIsGone: the release candidates'
// --controller-image is not a flag of bootstrap site.
func TestBootstrapSite_ControllerImageFlagIsGone(t *testing.T) {
	_, _, err := runGenerate(t, "bootstrap", "site", "-f", "unused.yaml", "--controller-image", "c:1")
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --controller-image") {
		t.Fatalf("err = %v, want --controller-image rejected as an unknown flag", err)
	}
}

// TestBootstrapSite_ArchNeedsARelease: --arch only picks a --release artifact,
// so an explicit one without --release is refused instead of ignored. The
// default does not count, which is why the check reads the flag's Changed.
func TestBootstrapSite_ArchNeedsARelease(t *testing.T) {
	_, _, err := runGenerate(t, "bootstrap", "site", "-f", "unused.yaml", "--arch", "linux/riscv64")
	if err == nil || !strings.Contains(err.Error(), "--arch") || !strings.Contains(err.Error(), "--release") {
		t.Fatalf("err = %v, want --arch refused without --release", err)
	}
}

func TestNvkeImages(t *testing.T) {
	tests := []struct {
		name     string
		nico     bool
		profiles string
		want     []string
	}{
		{"provisioned loads both images", false, "", []string{"cwe:1", "prov:1"}},
		{"nico loads only the CWE image", true, "", []string{"cwe:1"}},
		{"a profiles image is loaded too", false, "profiles:e2e", []string{"cwe:1", "prov:1", "profiles:e2e"}},
		{"nico with profiles loads the CWE and profiles images", true, "profiles:e2e", []string{"cwe:1", "profiles:e2e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := controllerdeploy.Config{CWEImage: "cwe:1", ProvisionedProviderImage: "prov:1", NicoInfrastructure: tt.nico, ProfilesImage: tt.profiles}
			if got := nvkeImages(cfg); !slices.Equal(got, tt.want) {
				t.Errorf("nvkeImages = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCWEDeployConfig_NicoTenantLoginAndReleases: the controller config carries
// the tenant login, read from NVKECTL_NODE_USER and NVKECTL_NODE_PASSWORD, and the release
// manifests' path resolved from the site spec, for the nico provider only.
func TestCWEDeployConfig_NicoTenantLoginAndReleases(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"nico.token"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("example-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := sitespec.Load(writeSiteSpec(t, validSiteSpec))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := cweDeployConfig(inv, "c", false, "", ""); err != nil || cfg.DGXCTenant != (controllerdeploy.TenantLogin{}) || cfg.ReleasesPath != "" {
		t.Fatalf("provisioned: %+v, %v; want no tenant login or releases", cfg, err)
	}

	t.Setenv(sitespec.NicoCredentialsEnv, filepath.Join(dir, "nico.token"))
	t.Setenv(stackconfig.EnvNodeUser, "example-user")
	t.Setenv(stackconfig.EnvNodePassword, "example-secret")
	nico := &sitespec.SiteSpec{
		Infra: sitespec.Infra{
			Provider: sitespec.ProviderNico,
			Nico: &sitespec.NicoInfra{
				OrgID:    "o",
				Remote:   &sitespec.NicoRemote{Endpoint: "https://nico.example"},
				Releases: filepath.Join(dir, "releases"),
			},
		},
	}
	cfg, err := cweDeployConfig(nico, "c", true, "", "")
	if err != nil {
		t.Fatalf("nico: %v", err)
	}
	if want := (controllerdeploy.TenantLogin{User: "example-user", Password: "example-secret"}); cfg.DGXCTenant != want {
		t.Errorf("DGXCTenant = %+v, want %+v", cfg.DGXCTenant, want)
	}
	if cfg.ReleasesPath != filepath.Join(dir, "releases") {
		t.Errorf("ReleasesPath = %q", cfg.ReleasesPath)
	}

	t.Setenv(stackconfig.EnvNodePassword, "")
	if _, err := cweDeployConfig(nico, "c", true, "", ""); err == nil || !strings.Contains(err.Error(), stackconfig.EnvNodePassword) {
		t.Errorf("unset node password: err = %v, want %s", err, stackconfig.EnvNodePassword)
	}
}

// TestDumpSiteSpecNamesTheNicoControlPlaneAsDocumented: the block embeds the
// API's struct, which has json tags only, so the dump has to write the API's
// camelCase names and leave out what is unset, not yaml.v3's lower-cased ones.
func TestDumpSiteSpecNamesTheNicoControlPlaneAsDocumented(t *testing.T) {
	inv, err := sitespec.Load(nicoSiteSpecFile(t))
	if err != nil {
		t.Fatalf("load site spec: %v", err)
	}
	var buf bytes.Buffer
	if err := dumpSiteSpec(&buf, inv); err != nil {
		t.Fatalf("dumpSiteSpec: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"count: 3", "instanceTypePreference:", "vpcPrefixIDs:", "sshKeyGroupIDs:", "release: example-ubuntu-24.04-amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dump lacks %q:\n%s", want, out)
		}
	}
	// The example sets vpcPrefixIDs and no subnetIDs, and leaves configProfile out.
	for _, reject := range []string{"sshkeygroupids", "vpcprefixids", "instancetypepreference", "configprofile", "subnetids", "subnetIDs", "configProfile"} {
		if strings.Contains(out, reject) {
			t.Errorf("the dump has %q, which is lower-cased or unset:\n%s", reject, out)
		}
	}
}

// TestBootstrapPortMappings_ControllerAPIIsLoopbackOnly: the bootstrap
// controller serves its API with no authentication, so Kind publishes it on
// 127.0.0.1 alone; the SPIRE port stays on every address for the nodes.
func TestBootstrapPortMappings_ControllerAPIIsLoopbackOnly(t *testing.T) {
	cfg := controllerdeploy.Config{CWEPort: 30080, NicoInfrastructure: true}
	got := bootstrapPortMappings(cfg)
	if len(got) != 2 {
		t.Fatalf("mappings = %+v, want the controller API and SPIRE", got)
	}
	if got[0].ContainerPort != 30080 || got[0].ListenAddress != "127.0.0.1" {
		t.Errorf("controller API mapping = %+v, want port 30080 on 127.0.0.1", got[0])
	}
	if got[1].ContainerPort != controllerdeploy.SPIREServerNodePort || got[1].ListenAddress != "" {
		t.Errorf("SPIRE mapping = %+v, want it on every address", got[1])
	}
	cfg.DisableMTLS = true
	if got := bootstrapPortMappings(cfg); len(got) != 1 {
		t.Errorf("mappings without the TPM attestor = %+v, want only the controller API", got)
	}
}

func TestRequireIsolatedRuntimeCNI(t *testing.T) {
	dir := t.TempDir()
	if err := requireIsolatedRuntimeCNI(dir); err != nil {
		t.Fatalf("an empty directory should pass: %v", err)
	}
	if err := requireIsolatedRuntimeCNI(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("a missing directory should pass: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "10-ovn-kubernetes.conf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireIsolatedRuntimeCNI(dir); err != nil {
		t.Fatalf("the cluster's own CNI config is not a runtime config: %v", err)
	}

	stale := filepath.Join(dir, "nerdctl-bridge.conflist")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := requireIsolatedRuntimeCNI(dir)
	if err == nil {
		t.Fatal("a nerdctl config in the node's CNI directory should fail the preflight")
	}
	if !strings.HasPrefix(err.Error(), dir+" already holds nerdctl network config(s)") {
		t.Errorf("error should start with the directory that holds the configs, got:\n%v", err)
	}
	for _, want := range []string{stale, "CoreDNS", `grep -h '"bridge":' ` + dir + "/nerdctl-*.conflist", "ip link delete", "cni_netconfpath", bootstrapengine.IsolatedCNIConfDir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
}

func TestBootstrapControlPlaneVIPOverrideReachesController(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPIWithNodes(t, true, 3, 3)
	runner := &recordingCWERunner{next: newFakeCWERunner(t)}
	body := strings.Replace(threeNodeSiteSpec, "  controlPlaneVIP: 10.0.0.100\n", "", 1)
	err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
		siteSpec:                    writeSiteSpec(t, body),
		bootstrapRunner:             fakeBootstrapRunner{},
		cweRunner:                   runner,
		kubeconfigOut:               filepath.Join(t.TempDir(), "site.kubeconfig"),
		lookPath:                    runtimeOnPath,
		dialContext:                 refuseDial,
		untaintControlPlane:         skipUntaint,
		dumpSpireEntries:            func(context.Context, string, string) (int, error) { return 0, nil },
		encryptionKeyOut:            filepath.Join(t.TempDir(), "key.json"),
		allowControlPlaneWithoutVIP: true,
		clusterTimeout:              5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !runner.saw("--set allowControlPlaneWithoutVIP=true") {
		t.Fatal("bootstrap did not enable the VIP override in the temporary controller")
	}
}

// runBootstrapDryRun runs "bootstrap site --dry-run" through execute with extra
// flags, returning stdout, stderr and the error execute reports. These tests
// run commands through execute, so none is parallel.
func runBootstrapDryRun(t *testing.T, extra ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runBootstrapDryRunEnv(t, nil, extra...)
}

// runBootstrapDryRunEnv is runBootstrapDryRun with environment variables set
// after the OTLP ones are cleared.
func runBootstrapDryRunEnv(t *testing.T, env map[string]string, extra ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// The OTLP configuration under test comes from flags alone, so a shell's own
	// does not leak in.
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
	} {
		t.Setenv(name, "")
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
	root := NewRootCmd("test", "test", "test", "test")
	root.SetArgs(append([]string{"bootstrap", "site", "-f", writeSiteSpec(t, validSiteSpec), "--dry-run"}, extra...))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)

	// The logger the command installs writes to the process's stderr, where the
	// export warning goes, so capture that too.
	r, w, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	realStderr, realLogger := os.Stderr, slog.Default()
	t.Cleanup(func() { os.Stderr = realStderr; slog.SetDefault(realLogger) })
	os.Stderr = w
	var logs bytes.Buffer
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(&logs, r); close(copied) }()
	err = execute(context.Background(), root, "test", &errOut)
	os.Stderr = realStderr
	slog.SetDefault(realLogger)
	require.NoError(t, w.Close())
	<-copied
	require.NoError(t, r.Close())
	return out.String(), errOut.String() + logs.String(), err
}

// exportWarning is the start of the warning a run logs, once, when it cannot
// export telemetry.
const exportWarning = "could not export telemetry"

// A flag value nvkectl cannot use is an error that names the flag, before
// bootstrap starts: --otlp-insecure without an endpoint, an endpoint that names
// no host or port or cannot be parsed, or one the temporary controller cannot
// reach.
func TestBootstrapSiteOTLPFlagErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"insecure without an endpoint", []string{"--otlp-insecure"}, "--otlp-endpoint"},
		{"insecure with an https endpoint", []string{"--otlp-endpoint", "https://collector.example:4317", "--otlp-insecure"}, "https"},
		{"unparsable endpoint", []string{"--otlp-endpoint", "http://"}, "--otlp-endpoint"},
		{"loopback", []string{"--otlp-endpoint", "localhost:4317"}, "loopback"},
		{"zoned loopback", []string{"--otlp-endpoint", "[::1%25lo0]:4317"}, "loopback"},
		{"no host", []string{"--otlp-endpoint", ":4317"}, "--otlp-endpoint"},
		{"no host with a scheme", []string{"--otlp-endpoint", "http://:4317"}, "--otlp-endpoint"},
		{"no port", []string{"--otlp-endpoint", "collector.example:"}, "--otlp-endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runBootstrapDryRun(t, tc.args...)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "--otlp-endpoint")
			assert.Contains(t, err.Error(), tc.want)
			assert.Regexp(t, `(?m)^Trace ID: [0-9a-f]{32}$`, err.Error(), "a failure still carries the trace ID")
		})
	}
}

// A bad endpoint in the environment is one warning that says what to do; on
// "bootstrap site" it also offers the flag. The command runs as without it.
func TestBootstrapSiteOTLPBadEnvironmentWarnsAndRuns(t *testing.T) {
	baseOut, _, err := runBootstrapDryRun(t)
	require.NoError(t, err)

	stdout, stderr, err := runBootstrapDryRunEnv(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:secret@"})

	require.NoError(t, err)
	assert.Equal(t, baseOut, stdout, "stdout is unchanged")
	assert.Equal(t, 1, strings.Count(stderr, "nvkectl sends no telemetry"), "one warning:\n%s", stderr)
	assert.Contains(t, stderr, "nvkectl sends no telemetry because OTEL_EXPORTER_OTLP_ENDPOINT is not a usable OTLP/gRPC endpoint; "+
		"fix or unset the variable, or pass --otlp-endpoint to export from this command")
	assert.NotContains(t, stderr, "secret")
}

func TestBootstrapSiteOTLPFlagUnreachableNeverFailsTheCommand(t *testing.T) {
	lis, err := net.Listen("tcp", nonLoopbackIPv4(t, false)+":0")
	require.NoError(t, err)
	closed := lis.Addr().String()
	require.NoError(t, lis.Close())

	baseOut, baseErr, err := runBootstrapDryRun(t)
	require.NoError(t, err)
	assert.Zero(t, strings.Count(baseErr, exportWarning), "no flag, no export warning")

	started := time.Now()
	stdout, stderr, err := runBootstrapDryRun(t, "--otlp-endpoint", closed, "--otlp-insecure")

	require.NoError(t, err)
	assert.Less(t, time.Since(started), 6*time.Second, "the flush is bounded")
	assert.Equal(t, baseOut, stdout, "stdout is unchanged")
	assert.Regexp(t, traceIDLineRE, stderr)
	assert.Equal(t, 1, strings.Count(stderr, exportWarning), "one warning for the unreachable endpoint:\n%s", stderr)
}

func TestBootstrapSiteOTLPFlagWinsOverEnvironment(t *testing.T) {
	flagCollector := newOTLPCollector(t)
	envCollector := newOTLPCollector(t)
	// The helper clears these variables first, so they are passed through it.
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + envCollector.addr,
		"OTEL_EXPORTER_OTLP_HEADERS":  "authorization=Bearer example-token",
	}

	_, _, err := runBootstrapDryRunEnv(t, env, "--otlp-endpoint", "http://"+flagCollector.addr)
	require.NoError(t, err)

	spans, md := flagCollector.received()
	assert.NotEmpty(t, spans, "the flag's collector receives the run's spans")
	assert.Equal(t, []string{"Bearer example-token"}, md.Get("authorization"), "the environment's headers still apply")
	envSpans, _ := envCollector.received()
	assert.Empty(t, envSpans, "the environment's endpoint receives nothing")
}

// controllerTracing hands the run's OTLP endpoint and headers to the controller
// deploy. A loopback endpoint is not handed over, and with it go no headers.
func TestControllerTracing(t *testing.T) {
	tests := []struct {
		name         string
		cfg          *telemetry.OTLPConfig
		want         controllerdeploy.TracingConfig
		wantWarnings int
		wantMessage  string
	}{
		{name: "export off"},
		{
			name: "endpoint",
			cfg:  &telemetry.OTLPConfig{Endpoint: "collector.example:4317", Insecure: true},
			want: controllerdeploy.TracingConfig{Endpoint: "collector.example:4317", Insecure: true},
		},
		{
			name: "endpoint with headers",
			cfg:  &telemetry.OTLPConfig{Endpoint: "collector.example:4317", Headers: map[string]string{"authorization": "Bearer example-token"}},
			want: controllerdeploy.TracingConfig{
				Endpoint: "collector.example:4317",
				Headers:  map[string]string{"authorization": "Bearer example-token"},
			},
		},
		{
			name:         "loopback endpoint from the environment",
			cfg:          &telemetry.OTLPConfig{Endpoint: "127.0.0.1:4317", Insecure: true, Headers: map[string]string{"authorization": "Bearer example-token"}},
			wantWarnings: 1,
			wantMessage:  "the controller in the bootstrap cluster cannot reach 127.0.0.1:4317; its spans are not exported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := telemetry.NewRun("test")
			if tt.cfg != nil {
				require.NoError(t, run.EnableOTLP(*tt.cfg))
			}
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			got := controllerTracing(telemetry.ContextWithRun(context.Background(), run), logger)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantWarnings, strings.Count(buf.String(), "level=WARN"), buf.String())
			if tt.wantWarnings > 0 {
				assert.Contains(t, buf.String(), tt.wantMessage)
			}
			assert.NotContains(t, buf.String(), "example-token", "a warning never holds a header value")
		})
	}
	// A context without a run, as a test or library caller has, exports nothing.
	assert.Equal(t, controllerdeploy.TracingConfig{}, controllerTracing(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))))
}

// flakySiteRunner fails the first controller install, as a dropped connection
// to the site's VIP does, and records the Postgres password each install passed.
type flakySiteRunner struct {
	controllerInstalls int
	passwords          []string
	// failure is the output of the failed install; a dropped connection when empty.
	failure string
}

func (r *flakySiteRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	switch {
	case strings.Contains(line, "get storageclass"):
		return "true\n", nil
	case strings.Contains(line, "get --raw"):
		return `{"status":"ok"}`, nil
	case strings.Contains(line, "upgrade --install postgres"):
		for i, arg := range args {
			if arg == "--values" && i+1 < len(args) {
				if b, err := os.ReadFile(args[i+1]); err == nil && strings.Contains(string(b), "password") {
					r.passwords = append(r.passwords, string(b))
				}
			}
		}
	case strings.Contains(line, "upgrade --install nke-controller"):
		r.controllerInstalls++
		if r.controllerInstalls == 1 {
			if r.failure != "" {
				return r.failure, errors.New("exit status 1")
			}
			return "write tcp 192.0.2.10:54586->192.0.2.100:6443: write: broken pipe", errors.New("exit status 1")
		}
	}
	return "", nil
}

func TestDeploySiteStackRetriesWithTheSamePassword(t *testing.T) {
	runner := &flakySiteRunner{}
	cfg := controllerdeploy.Config{
		CWEImage: "registry.example.com/nvke-controller:1", ProvisionedProviderImage: "registry.example.com/provisioned:1",
		DisableMTLS: true, ReadyTimeout: time.Second,
	}
	inv := &sitespec.SiteSpec{ClusterName: "site", Cluster: sitespec.Cluster{ControlPlaneVIP: "192.0.2.100"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := deploySiteStack(context.Background(), inv, "site.kubeconfig", cfg, runner, logger); err != nil {
		t.Fatalf("deploySiteStack: %v", err)
	}
	if runner.controllerInstalls != 2 {
		t.Fatalf("controller installs = %d, want a retry after the failed one", runner.controllerInstalls)
	}
	if len(runner.passwords) != 2 || runner.passwords[0] != runner.passwords[1] {
		t.Fatalf("each attempt must pass Postgres the same password, got %d differing values", len(runner.passwords))
	}
}

func TestDeploySiteStackReportsOtherFailuresAtOnce(t *testing.T) {
	runner := &flakySiteRunner{failure: "Error: context deadline exceeded"}
	cfg := controllerdeploy.Config{
		CWEImage: "registry.example.com/nvke-controller:1", ProvisionedProviderImage: "registry.example.com/provisioned:1",
		DisableMTLS: true, ReadyTimeout: time.Second,
	}
	inv := &sitespec.SiteSpec{ClusterName: "site", Cluster: sitespec.Cluster{ControlPlaneVIP: "192.0.2.100"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := deploySiteStack(context.Background(), inv, "site.kubeconfig", cfg, runner, logger); err == nil {
		t.Fatal("deploySiteStack succeeded, want the failed install reported")
	}
	if runner.controllerInstalls != 1 {
		t.Fatalf("controller installs = %d, want no retry after a failure that is not a lost connection", runner.controllerInstalls)
	}
}
