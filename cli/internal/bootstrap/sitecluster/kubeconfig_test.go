// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	certsv1client "k8s.io/client-go/kubernetes/typed/certificates/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestIssueMintsBreakglassKubeconfig(t *testing.T) {
	client := fake.NewSimpleClientset()
	signCSRs(client)

	got, err := issue(context.Background(), client, adminKubeconfig(t), "nvke-site")
	if err != nil {
		t.Fatalf("issue() error = %v, want nil", err)
	}

	cfg, err := clientcmd.Load(got)
	if err != nil {
		t.Fatalf("load issued kubeconfig: %v", err)
	}
	cluster := cfg.Clusters["cluster"]
	if cluster == nil {
		t.Fatal("issued kubeconfig has no cluster")
	}
	if cluster.Server != "https://site.example:6443" {
		t.Errorf("server = %q", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != "test-ca" {
		t.Errorf("ca = %q, want test-ca", cluster.CertificateAuthorityData)
	}
	user := cfg.AuthInfos["nvke-breakglass"]
	if user == nil {
		t.Fatal("issued kubeconfig has no nvke-breakglass user")
	}
	if len(user.ClientKeyData) == 0 {
		t.Fatal("issued kubeconfig has no client private key")
	}
	if string(user.ClientKeyData) == "admin-key" {
		t.Fatal("issued kubeconfig copied the admin private key")
	}
	if !strings.Contains(string(user.ClientCertificateData), "BEGIN CERTIFICATE") {
		t.Errorf("client cert = %q", user.ClientCertificateData)
	}

	binding, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), BindingName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ClusterRoleBinding: %v", err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != rbacv1.GroupKind || binding.Subjects[0].Name != GroupName {
		t.Errorf("binding subjects = %#v", binding.Subjects)
	}
	if binding.RoleRef.Name != "cluster-admin" {
		t.Errorf("roleRef = %#v", binding.RoleRef)
	}

	if !sawCSR(client, "nvke-site") {
		t.Fatal("no kube-apiserver-client CSR with O=nvke:breakglass")
	}
	assertNoCSRs(t, client)
}

func TestIssueIsIdempotentOnExistingBinding(t *testing.T) {
	existing := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: BindingName},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
	}
	client := fake.NewSimpleClientset(existing)
	signCSRs(client)

	if _, err := issue(context.Background(), client, adminKubeconfig(t), "nvke-site"); err != nil {
		t.Fatalf("issue() error = %v, want nil", err)
	}
}

func TestIssueReportsDeniedCSR(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "certificatesigningrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		return true, &certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{Name: get.GetName()},
			Status: certificatesv1.CertificateSigningRequestStatus{
				Conditions: []certificatesv1.CertificateSigningRequestCondition{{
					Type:    certificatesv1.CertificateDenied,
					Status:  corev1.ConditionTrue,
					Message: "nope",
				}},
			},
		}, nil
	})

	_, err := issue(context.Background(), client, adminKubeconfig(t), "nvke-site")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("issue() error = %v, want denied CSR", err)
	}
	assertNoCSRs(t, client)
}

func TestIssueRetriesTransientCSRGet(t *testing.T) {
	client := fake.NewSimpleClientset()
	gets := 0
	client.PrependReactor("get", "certificatesigningrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return true, nil, apierrors.NewTooManyRequests("slow down", 1)
		}
		get := action.(k8stesting.GetAction)
		return true, &certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{Name: get.GetName()},
			Status: certificatesv1.CertificateSigningRequestStatus{
				Certificate: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
			},
		}, nil
	})

	if _, err := issue(context.Background(), client, adminKubeconfig(t), "nvke-site"); err != nil {
		t.Fatalf("issue() error = %v, want nil", err)
	}
	if gets < 2 {
		t.Fatalf("gets = %d, want at least one retry after TooManyRequests", gets)
	}
}

func TestWaitForCertificateSurfacesLastTransientError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTooManyRequests("slow down", 1)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := waitForCertificate(ctx, client, "nvke-breakglass-test")
	if err == nil || !apierrors.IsTooManyRequests(err) {
		t.Fatalf("waitForCertificate() error = %v, want too many requests", err)
	}
}

func TestIssueReportsPermanentCSRGetError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "certificates.k8s.io", Resource: "certificatesigningrequests"},
			"",
			errors.New("nope"),
		)
	})

	_, err := issue(context.Background(), client, adminKubeconfig(t), "nvke-site")
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("issue() error = %v, want forbidden", err)
	}
	assertNoCSRs(t, client)
}

// TestIssueDeletesCSRWhenWaitTimesOut creates a CSR, wait hung until the context is cancelled
// and then verifies delete cleared CSR with cancelled context.
func TestIssueDeletesCSRWhenWaitTimesOut(t *testing.T) {
	inner := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waiting := make(chan struct{})
	var once sync.Once
	inner.PrependReactor("get", "certificatesigningrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// We got the CSR, now we inform and block for context to be cancelled.
		once.Do(func() { close(waiting) })
		<-ctx.Done()
		get := action.(k8stesting.GetAction)
		return true, &certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{Name: get.GetName()},
		}, nil
	})

	errc := make(chan error, 1)
	go func() {
		_, err := issue(ctx, liveDeleteClient{inner}, adminKubeconfig(t), "nvke-site")
		errc <- err
	}()

	// Make sure we get the CSR first, before issue returns
	select {
	case <-waiting:
	case err := <-errc:
		t.Fatalf("issue() returned before wait: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CSR Get")
	}
	cancel()

	err := <-errc
	if err == nil {
		t.Fatal("issue() error = nil, want wait timeout")
	}
	if !sawCSR(inner, "nvke-site") {
		t.Fatal("no CSR was created before wait timed out")
	}
	assertNoCSRs(t, inner)
}

// liveDeleteClient is a fake whose CSR Delete refuses a cancelled context, matching
// a real client-go REST client. The tracker alone would delete even with a dead ctx.
type liveDeleteClient struct {
	kubernetes.Interface
}

func (c liveDeleteClient) CertificatesV1() certsv1client.CertificatesV1Interface {
	return liveDeleteCertsV1{c.Interface.CertificatesV1()}
}

type liveDeleteCertsV1 struct {
	certsv1client.CertificatesV1Interface
}

func (c liveDeleteCertsV1) CertificateSigningRequests() certsv1client.CertificateSigningRequestInterface {
	return liveDeleteCSRs{c.CertificatesV1Interface.CertificateSigningRequests()}
}

type liveDeleteCSRs struct {
	certsv1client.CertificateSigningRequestInterface
}

func (c liveDeleteCSRs) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.CertificateSigningRequestInterface.Delete(ctx, name, opts)
}

func TestGenerateBreakglassKubeconfigRejectsGarbage(t *testing.T) {
	_, err := GenerateBreakglassKubeconfig(context.Background(), []byte("not a kubeconfig"), "nvke-site")
	if err == nil {
		t.Fatal("GenerateBreakglassKubeconfig() error = nil, want parse error")
	}
}

func adminKubeconfig(t *testing.T) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   "https://site.example:6443",
		CertificateAuthorityData: []byte("test-ca"),
	}
	cfg.AuthInfos["kubernetes-admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: []byte("admin-cert"),
		ClientKeyData:         []byte("admin-key"),
	}
	cfg.Contexts["admin"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "kubernetes-admin"}
	cfg.CurrentContext = "admin"
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("write admin kubeconfig: %v", err)
	}
	return raw
}

func signCSRs(client *fake.Clientset) {
	client.PrependReactor("get", "certificatesigningrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		return true, &certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{Name: get.GetName()},
			Status: certificatesv1.CertificateSigningRequestStatus{
				Certificate: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
			},
		}, nil
	})
}

func assertNoCSRs(t *testing.T, client *fake.Clientset) {
	t.Helper()
	list, err := client.CertificatesV1().CertificateSigningRequests().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list CSRs: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("leftover CSRs: %d", len(list.Items))
	}
}

func sawCSR(client *fake.Clientset, clusterName string) bool {
	for _, action := range client.Actions() {
		create, ok := action.(k8stesting.CreateAction)
		if !ok || action.GetResource().Resource != "certificatesigningrequests" {
			continue
		}
		csr, ok := create.GetObject().(*certificatesv1.CertificateSigningRequest)
		if !ok {
			continue
		}
		if csr.Spec.SignerName != certificatesv1.KubeAPIServerClientSignerName {
			continue
		}
		block, _ := pem.Decode(csr.Spec.Request)
		if block == nil {
			continue
		}
		req, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			continue
		}
		if req.Subject.CommonName == "nvke:breakglass:"+clusterName &&
			len(req.Subject.Organization) == 1 && req.Subject.Organization[0] == GroupName {
			return true
		}
	}
	return false
}
