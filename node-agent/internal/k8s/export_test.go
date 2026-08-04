package k8s

import "k8s.io/client-go/rest"

func ConfigureDynamicClientRESTConfigForTest(cfg *rest.Config) {
	configureDynamicClientRESTConfig(cfg)
}

func EncryptRewriteWorkersForTest() int {
	return encryptRewriteWorkers
}
