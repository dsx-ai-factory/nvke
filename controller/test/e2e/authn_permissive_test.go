// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

// Permissive mode answers 200 to everything, so the counter is the only evidence
// it produces.
//
// The spec puts the deployment into permissive mode itself and puts it back
// afterwards, so it runs in the same lane as the enforcing specs rather than
// needing a cluster of its own. Ordered, because that flip is container-wide
// setup rather than per-spec.
var _ = Describe("NVKE API authentication in permissive mode", Ordered, Label("authn-permissive"), func() {
	var (
		restConfig *rest.Config
		clientset  kubernetes.Interface
		metrics    *kube.ServiceForwarder
		audience   string
	)

	BeforeAll(func(ctx SpecContext) {
		restConfig, clientset = authnCluster()

		var (
			found bool
			mode  string
		)
		audience, found, mode = authnDeployment(ctx, clientset)
		if !found {
			authnAbsentf(
				"authentication is OFF: ConfigMap %s/%s does not exist, so there is no audience to mint against and nothing to count",
				controller.Namespace(), authtoken.ConfigMapName,
			)
		}
		if mode == authnModeDisabled {
			authnAbsentf(
				"authentication is OFF: %s is not set on Deployment %s/%s, so there is no middleware to put into permissive mode",
				authnConfigPathEnv, controller.Namespace(), controller.Deployment(),
			)
		}

		runtimeClient, err := kube.RuntimeClient(restConfig)
		Expect(err).NotTo(HaveOccurred(), "build a controller-runtime client")

		rolloutCtx, rolloutCancel := context.WithTimeout(ctx, permissiveRolloutTimeout)
		defer rolloutCancel()
		previous, written, err := nvke.SetPermissiveMode(rolloutCtx, runtimeClient, true)
		// Registered before the error check: a restart that fails after the
		// ConfigMap write still leaves the deployment permissive.
		if written {
			DeferCleanup(func(ctx SpecContext) {
				restoreCtx, restoreCancel := context.WithTimeout(ctx, permissiveRolloutTimeout)
				defer restoreCancel()

				_, _, restoreErr := nvke.SetPermissiveMode(restoreCtx, runtimeClient, previous)
				Expect(restoreErr).NotTo(HaveOccurred(), "restore permissiveMode=%t", previous)

				// Checked, not assumed. The enforcing specs skip themselves while the
				// deployment is permissive, so a restore that quietly failed would turn
				// the rest of the authn pass green without running any of it.
				mode, modeErr := detectAuthnEnforcement(restoreCtx, clientset)
				Expect(modeErr).NotTo(HaveOccurred(), "re-read the enforcement mode after restoring it")
				Expect(mode).NotTo(Equal(authnModePermissive),
					"the deployment was left permissive; every enforcing spec after this one would skip")
			})
		}
		Expect(err).NotTo(HaveOccurred(), "put Deployment %s/%s into permissive mode",
			controller.Namespace(), controller.Deployment())

		tctx, cancel := context.WithTimeout(ctx, authnTimeout)
		defer cancel()

		// The metrics listener is a second server on a second port, reachable
		// only because the Service publishes `metrics` alongside `http`. The
		// probe makes a missing port say so rather than read as a dead NVKE.
		metrics, err = kube.SharedServiceForwarder(restConfig, controller.Namespace(), controller.Service(), nvkeControllerMetricsPort)
		Expect(err).NotTo(HaveOccurred(), "build a port-forward to the metrics listener")

		_, _, err = metrics.Get(tctx, metricsPath)
		Expect(err).NotTo(HaveOccurred(),
			"could not reach %s on the %q port of Service %s/%s; the chart publishes it from .Values.metrics.port and a Service without it makes the permissive counter unassertable",
			metricsPath, nvkeControllerMetricsPort, controller.Namespace(), controller.Service())
	})

	It("serves the three rejection cases and counts them as would_reject", func(ctx SpecContext) {
		before := authWouldRejectCount(ctx, metrics)

		// The same cases authn_test.go asserts 401, 401 and 403 for.
		wrongAudience, err := authtoken.New(clientset, authtoken.Options{Audience: authnWrongAudience})
		Expect(err).NotTo(HaveOccurred(), "build the wrong-audience token source")

		unadmitted, err := authtoken.New(clientset, authtoken.Options{
			Namespace:      authnOutsiderNamespace,
			ServiceAccount: authnOutsiderServiceAccount,
			Audience:       audience,
		})
		Expect(err).NotTo(HaveOccurred(), "build the unadmitted token source")

		for _, probe := range []struct {
			name   string
			client *nvke.Client
		}{
			{"no token", authnClient(restConfig, nil)},
			{"wrong audience", authnClient(restConfig, wrongAudience)},
			{"unadmitted service account", authnClient(restConfig, unadmitted)},
		} {
			status, body, _ := authnListClusters(ctx, probe.client)
			Expect(status).To(Equal(http.StatusOK),
				"%s: permissive mode evaluates and then serves, so this is a 200 and not a rejection; response body: %s",
				probe.name, body)
		}

		// Exactly three: an operator decides whether it is safe to enforce by
		// reading this number, so double-counting matters.
		Eventually(func() int {
			return authWouldRejectCount(ctx, metrics) - before
		}).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(Equal(3),
			"three requests that enforcement would have refused must advance nvke_auth_requests_total{outcome=\"would_reject\"} by three")

		// The admitted caller must not be counted, or a middleware that counted
		// every request would pass everything above.
		admitted, err := authtoken.New(clientset, authtoken.Options{Audience: audience})
		Expect(err).NotTo(HaveOccurred(), "build the admitted token source")

		status, body, _ := authnListClusters(ctx, authnClient(restConfig, admitted))
		Expect(status).To(Equal(http.StatusOK), "response body: %s", body)

		Consistently(func() int {
			return authWouldRejectCount(ctx, metrics) - before
		}).WithTimeout(6*time.Second).WithPolling(2*time.Second).Should(Equal(3),
			"an admitted caller is authenticated, not would_reject")
	})
})

// nvkeControllerMetricsPort is the port name the metrics listener is published on. A name
// and not 9090, because the chart renders the number from .Values.metrics.port.
const nvkeControllerMetricsPort = "metrics"

const metricsPath = "/metrics"

// permissiveRolloutTimeout bounds one flip of permissiveMode and the restart
// that follows it.
const permissiveRolloutTimeout = 6 * time.Minute

// The counter permissive mode's behavior lands in, and the label separating it
// from authenticated and from enforced rejections.
const (
	authMetricName   = "nvke_auth_requests_total"
	wouldRejectLabel = `outcome="would_reject"`
)

func authWouldRejectCount(ctx context.Context, metrics *kube.ServiceForwarder) int {
	GinkgoHelper()
	tctx, cancel := context.WithTimeout(ctx, authnTimeout)
	defer cancel()

	status, body, err := metrics.Get(tctx, metricsPath)
	Expect(err).NotTo(HaveOccurred(), "GET %s", metricsPath)
	Expect(status).To(Equal(http.StatusOK), "GET %s returned %d: %s", metricsPath, status, body)

	total, err := sumWouldRejectCounter(string(body))
	Expect(err).NotTo(HaveOccurred(), "parse %s{%s} from the exposition output", authMetricName, wouldRejectLabel)
	return total
}

// sumWouldRejectCounter adds up every sample of nvke_auth_requests_total carrying
// wouldRejectLabel. A sum, because the counter is also labeled by reason and
// issuer, so the spec's three cases land in three series. Substring, because
// client_golang sorts label pairs and outcome lands between issuer and reason.
// No matching series is zero: a fresh pod has none.
func sumWouldRejectCounter(exposition string) (int, error) {
	const label = wouldRejectLabel
	total := 0
	for _, line := range strings.Split(exposition, "\n") {
		openIdx := strings.Index(line, "{")
		closeIdx := strings.LastIndex(line, "}")
		if openIdx < 0 || closeIdx < openIdx || line[:openIdx] != authMetricName {
			continue
		}
		if !strings.Contains(line[openIdx+1:closeIdx], label) {
			continue
		}
		// `name{labels} value [timestamp]`, and counters are floats on the wire.
		fields := strings.Fields(line[closeIdx+1:])
		if len(fields) == 0 {
			return 0, fmt.Errorf("malformed exposition line %q", line)
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0, fmt.Errorf("parsing the value of %q: %w", line, err)
		}
		total += int(value)
	}
	return total, nil
}

// A parser that silently returned zero would make the delta assertion above
// pass for an NVKE that counted nothing.
func TestSumWouldRejectCounter(t *testing.T) {
	const exposition = `# HELP nvke_auth_requests_total Total API requests evaluated by authentication.
# TYPE nvke_auth_requests_total counter
nvke_auth_requests_total{issuer="local-kubernetes",outcome="authenticated",reason=""} 7
nvke_auth_requests_total{issuer="local-kubernetes",outcome="would_reject",reason="not_admitted"} 1
nvke_auth_requests_total{issuer="local-kubernetes",outcome="would_reject",reason="audience_mismatch"} 1
nvke_auth_requests_total{issuer="unknown",outcome="would_reject",reason="no_token"} 1
nvke_auth_requests_total{issuer="unknown",outcome="rejected",reason="no_token"} 4
`

	// Returning 3 from an exposition whose samples total 14 is also what proves
	// the label filter is applied at all.
	t.Run("sums every would_reject series", func(t *testing.T) {
		got, err := sumWouldRejectCounter(exposition)
		if err != nil {
			t.Fatalf("sumWouldRejectCounter: %v", err)
		}
		// Three series, one each; reading any single one would give 1.
		if got != 3 {
			t.Fatalf("sumWouldRejectCounter = %d, want 3", got)
		}
	})

	t.Run("does not match another counter with the same suffix", func(t *testing.T) {
		// A prefix match would fold the _shadow series into the total.
		shadowed := exposition + authMetricName + `_shadow{outcome="would_reject"} 99` + "\n"
		got, err := sumWouldRejectCounter(shadowed)
		if err != nil {
			t.Fatalf("sumWouldRejectCounter: %v", err)
		}
		if got != 3 {
			t.Fatalf("sumWouldRejectCounter = %d, want 3", got)
		}
	})

	t.Run("no matching series is zero", func(t *testing.T) {
		got, err := sumWouldRejectCounter("# no samples yet\n")
		if err != nil {
			t.Fatalf("sumWouldRejectCounter: %v", err)
		}
		if got != 0 {
			t.Fatalf("sumWouldRejectCounter = %d, want 0", got)
		}
	})

	t.Run("a value that is not a number is an error", func(t *testing.T) {
		// Returning zero would read as "nothing was counted", which is what a
		// broken deployment produces.
		broken := authMetricName + `{outcome="would_reject",reason="x"} NaNsense` + "\n"
		if _, err := sumWouldRejectCounter(broken); err == nil {
			t.Fatal("expected an error for an unparseable sample value")
		}
	})
}
