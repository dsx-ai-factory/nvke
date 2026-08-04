package k8s

import (
	"os"
	"sync"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// AdminKubeconfigPath is the kubeadm admin kubeconfig on control-plane nodes.
const AdminKubeconfigPath = "/etc/kubernetes/admin.conf"

const (
	// The rewrite client uses kubeadm's admin.conf credentials. Kubernetes
	// client-cert auth maps Subject.CommonName to username and Subject.Organization
	// ("O") to groups; RBAC and APF then use that authenticated user.Info.
	// See k/k CommonNameUserConversion:
	// https://github.com/kubernetes/kubernetes/blob/v1.34.2/staging/src/k8s.io/apiserver/pkg/authentication/request/x509/x509.go
	//
	// For kubeadm-provisioned clusters admin.conf is CN=kubernetes-admin,
	// O=kubeadm:cluster-admins, not O=system:masters. The built-in APF exempt
	// FlowSchema only matches system:masters, so rewrite traffic normally falls
	// through to the global-default FlowSchema via system:authenticated.
	// See k/k APF bootstrap defaults:
	// https://github.com/kubernetes/kubernetes/blob/v1.34.2/staging/src/k8s.io/apiserver/pkg/apis/flowcontrol/bootstrap/default.go
	//
	// With stock apiserver flags (400 read-only + 200 mutating = 600 seats) and
	// stock APF shares, global-default has nominally ceil(600*20/245)=49 seats
	// per apiserver. APF borrowing/lending can move the current limit; the
	// apiserver_flowcontrol_*_limit_seats metrics are authoritative at runtime.
	dynamicClientQPS   float32 = 1000
	dynamicClientBurst int     = 1000
)

var (
	dynOnce          sync.Once
	dynamicClient    dynamic.Interface
	dynamicClientErr error

	// overrideDynamicClient, if non-nil, is returned by getDynamicClient (tests).
	overrideDynamicClient dynamic.Interface
)

// SetDynamicClientForTest substitutes the dynamic client returned by getDynamicClient.
// Pass nil to use the default client built from the admin kubeconfig.
func SetDynamicClientForTest(c dynamic.Interface) {
	overrideDynamicClient = c
}

func configureDynamicClientRESTConfig(cfg *rest.Config) {
	cfg.QPS = dynamicClientQPS
	cfg.Burst = dynamicClientBurst
}

func getDynamicClient() (dynamic.Interface, error) {
	if overrideDynamicClient != nil {
		return overrideDynamicClient, nil
	}
	dynOnce.Do(func() {
		kubeconfig, err := os.ReadFile(AdminKubeconfigPath)
		if err != nil {
			dynamicClientErr = err
			return
		}
		cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if err != nil {
			dynamicClientErr = err
			return
		}
		configureDynamicClientRESTConfig(cfg)
		dynamicClient, dynamicClientErr = dynamic.NewForConfig(cfg)
	})
	return dynamicClient, dynamicClientErr
}
