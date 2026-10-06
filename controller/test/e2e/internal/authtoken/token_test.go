// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authtoken

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/controller"
)

// fakeClock crosses the refresh boundary without sleeping through a lifetime.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type mintedToken struct {
	serviceAccount string
	namespace      string
	audiences      []string
	expirationSecs int64
}

type tokenRecorder struct {
	mu       sync.Mutex
	requests []mintedToken
	clock    *fakeClock
	granted  time.Duration
	// err, when set, is returned instead of a token.
	err error
}

func (r *tokenRecorder) install(client *fake.Clientset) {
	client.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateActionImpl)
		if !ok || create.Subresource != "token" {
			return false, nil, nil
		}
		request, ok := create.Object.(*authnv1.TokenRequest)
		if !ok {
			return true, nil, fmt.Errorf("unexpected create object %T", create.Object)
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		if r.err != nil {
			return true, nil, r.err
		}

		expiration := int64(0)
		if request.Spec.ExpirationSeconds != nil {
			expiration = *request.Spec.ExpirationSeconds
		}
		r.requests = append(r.requests, mintedToken{
			serviceAccount: create.Name,
			namespace:      create.Namespace,
			audiences:      request.Spec.Audiences,
			expirationSecs: expiration,
		})

		granted := cmp.Or(r.granted, time.Duration(expiration)*time.Second)
		return true, &authnv1.TokenRequest{
			Status: authnv1.TokenRequestStatus{
				Token:               fmt.Sprintf("token-%d", len(r.requests)),
				ExpirationTimestamp: metav1.NewTime(r.clock.Now().Add(granted)),
			},
		}, nil
	})
}

func (r *tokenRecorder) calls() []mintedToken {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mintedToken(nil), r.requests...)
}

func newTestSource(t *testing.T) (*Source, *tokenRecorder, *fakeClock) {
	t.Helper()

	clock := &fakeClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	client := fake.NewClientset()
	recorder := &tokenRecorder{clock: clock}
	recorder.install(client)

	source, err := New(client, Options{
		Audience: "https://nke.tilt.local",
		Now:      clock.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return source, recorder, clock
}

func TestTokenMintsWithTheConfiguredIdentityAndAudience(t *testing.T) {
	source, recorder, _ := newTestSource(t)

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "token-1" {
		t.Fatalf("token = %q, want token-1", token)
	}

	calls := recorder.calls()
	if len(calls) != 1 {
		t.Fatalf("minted %d tokens, want 1", len(calls))
	}
	if calls[0].namespace != DefaultNamespace || calls[0].serviceAccount != DefaultServiceAccount {
		t.Errorf("minted for %s/%s, want %s/%s", calls[0].namespace, calls[0].serviceAccount, DefaultNamespace, DefaultServiceAccount)
	}
	if len(calls[0].audiences) != 1 || calls[0].audiences[0] != "https://nke.tilt.local" {
		t.Errorf("audiences = %v, want [https://nke.tilt.local]", calls[0].audiences)
	}
	if want := int64(DefaultTTL.Seconds()); calls[0].expirationSecs != want {
		t.Errorf("expirationSeconds = %d, want %d", calls[0].expirationSecs, want)
	}
}

func TestTokenIsCachedUntilTheRefreshMargin(t *testing.T) {
	source, recorder, clock := newTestSource(t)
	ctx := context.Background()

	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}

	// Well inside the lifetime: the same token comes back and nothing is minted.
	clock.advance(DefaultTTL - DefaultRefreshBefore - time.Minute)
	token, err := source.Token(ctx)
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if token != "token-1" {
		t.Fatalf("token = %q, want the cached token-1", token)
	}
	if got := len(recorder.calls()); got != 1 {
		t.Fatalf("minted %d tokens, want 1 while the cached one is still fresh", got)
	}
}

func TestTokenRefreshesBeforeExpiry(t *testing.T) {
	source, recorder, clock := newTestSource(t)
	ctx := context.Background()

	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}

	// Inside the refresh margin but not yet expired.
	clock.advance(DefaultTTL - DefaultRefreshBefore + time.Second)
	token, err := source.Token(ctx)
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if token != "token-2" {
		t.Fatalf("token = %q, want a re-minted token-2", token)
	}
	if got := len(recorder.calls()); got != 2 {
		t.Fatalf("minted %d tokens, want 2", got)
	}
}

func TestTokenSurvivesALifetimeShorterThanTheRefreshMargin(t *testing.T) {
	// A grant shorter than the refresh margin; without backing off, every
	// request would mint.
	source, recorder, clock := newTestSource(t)
	recorder.granted = 2 * time.Minute // shorter than DefaultRefreshBefore
	ctx := context.Background()

	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	clock.advance(30 * time.Second)
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if got := len(recorder.calls()); got != 1 {
		t.Fatalf("minted %d tokens, want 1: a short grant must still be cached", got)
	}

	// Past half the granted lifetime, it refreshes.
	clock.advance(45 * time.Second)
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("third Token: %v", err)
	}
	if got := len(recorder.calls()); got != 2 {
		t.Fatalf("minted %d tokens, want 2 after the backed-off margin elapsed", got)
	}
}

// The margin is a property of each grant: a clamped one writing its
// half-lifetime back would hold later tokens until seconds before expiry.
func TestTokenDoesNotCarryOneShortGrantsMarginIntoTheNext(t *testing.T) {
	source, recorder, clock := newTestSource(t)
	ctx := context.Background()

	recorder.granted = 2 * time.Minute // shorter than DefaultRefreshBefore
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}

	// The clamp goes away and a full lifetime is granted.
	recorder.granted = DefaultTTL
	clock.advance(2 * time.Minute)
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if got := len(recorder.calls()); got != 2 {
		t.Fatalf("minted %d tokens, want 2 once the short grant had expired", got)
	}

	// Inside the configured margin, not the one the short grant implied.
	clock.advance(DefaultTTL - DefaultRefreshBefore + time.Second)
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("third Token: %v", err)
	}
	if got := len(recorder.calls()); got != 3 {
		t.Fatalf("minted %d tokens, want 3: a full-lifetime grant was held past its refresh margin because an earlier short grant narrowed it", got)
	}
}

// A grant that arrives already expired is what a badly skewed clock looks like.
// It must not be handed out, and its negative lifetime must not become a
// negative margin that puts later refreshes after their expiry.
func TestTokenRefusesAGrantThatArrivesExpired(t *testing.T) {
	source, recorder, clock := newTestSource(t)
	ctx := context.Background()

	// Refused, so the error names clock skew rather than the caller seeing a
	// 401 with nothing pointing at the cause.
	recorder.granted = -time.Minute
	_, err := source.Token(ctx)
	if err == nil {
		t.Fatal("first Token: an already-expired grant must not be handed to a caller")
	}
	if !strings.Contains(err.Error(), "clock skew") {
		t.Errorf("first Token: error should name the likely cause, got %v", err)
	}

	// Still not cached: the next call mints again.
	if _, err := source.Token(ctx); err == nil {
		t.Fatal("second Token: expected the same refusal")
	}
	if got := len(recorder.calls()); got != 2 {
		t.Fatalf("minted %d tokens, want 2: an already-expired grant must never be cached", got)
	}

	recorder.granted = DefaultTTL
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("third Token: %v", err)
	}
	clock.advance(DefaultTTL - time.Second)
	if _, err := source.Token(ctx); err != nil {
		t.Fatalf("fourth Token: %v", err)
	}
	if got := len(recorder.calls()); got != 4 {
		t.Fatalf("minted %d tokens, want 4: a token one second from expiry was still being handed out", got)
	}
}

func TestTokenErrorNamesTheRBACItNeeds(t *testing.T) {
	source, recorder, _ := newTestSource(t)
	recorder.err = apierrors.NewForbidden(
		schema.GroupResource{Resource: "serviceaccounts"}, "nke-client",
		errors.New("cannot create resource \"serviceaccounts/token\""),
	)

	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("Token succeeded, want a forbidden error")
	}
	for _, want := range []string{"nke-clients/nke-client", "https://nke.tilt.local", "serviceaccounts/token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewRejectsAnEmptyAudience(t *testing.T) {
	if _, err := New(fake.NewClientset(), Options{}); err == nil {
		t.Fatal("New succeeded with no audience, want an error")
	}
}

// The apiserver rejects a shorter request rather than raising it, so a mistyped
// TTL has to fail here and not inside a spec.
func TestNewRejectsALifetimeBelowTheKubernetesFloor(t *testing.T) {
	_, err := New(fake.NewClientset(), Options{Audience: "https://nke.tilt.local", TTL: 30 * time.Second})
	if err == nil {
		t.Fatal("New succeeded with a 30s lifetime, want an error")
	}
	if !strings.Contains(err.Error(), MinTTL.String()) {
		t.Errorf("error %q does not name the %s minimum", err, MinTTL)
	}

	if _, err := New(fake.NewClientset(), Options{Audience: "https://nke.tilt.local", TTL: MinTTL}); err != nil {
		t.Errorf("New rejected the minimum lifetime itself: %v", err)
	}
}

func TestAudienceFromCluster(t *testing.T) {
	tests := []struct {
		name    string
		data    map[string]string
		absent  bool
		want    string
		wantOK  bool
		wantErr bool
		errFrag string
	}{
		{
			// Any entry would authenticate; the first is taken to be predictable.
			// Flow versus block sequence is the YAML decoder's concern.
			name: "takes the first of several audiences",
			data: map[string]string{ConfigMapKey: "apiVersion: auth.nke.nvidia.com/v1alpha1\n" +
				"kind: AuthenticationConfiguration\n" +
				"audiences: [\"https://nke.tilt.local\", \"https://cwe.next.local\"]\n"},
			want:   "https://nke.tilt.local",
			wantOK: true,
		},
		{
			name:    "an empty audiences list is an error, not a default",
			data:    map[string]string{ConfigMapKey: "audiences: []\n"},
			wantErr: true,
			errFrag: "sets no top-level audiences",
		},
		{
			name:   "a missing ConfigMap means authentication is not configured",
			absent: true,
			wantOK: false,
		},
		{
			name:    "a ConfigMap without the auth key is an error, not a default",
			data:    map[string]string{"other.yaml": "audience: x"},
			wantErr: true,
			errFrag: "has no \"auth.yaml\" key",
		},
		{
			name:    "unparseable YAML is an error",
			data:    map[string]string{ConfigMapKey: "audience: [unterminated\n"},
			wantErr: true,
			errFrag: "parsing auth.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := []runtime.Object{}
			if !tc.absent {
				objects = append(objects, &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: controller.Namespace()},
					Data:       tc.data,
				})
			}
			client := fake.NewClientset(objects...)

			audience, ok, err := AudienceFromCluster(context.Background(), client)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("AudienceFromCluster succeeded, want an error")
				}
				if !strings.Contains(err.Error(), tc.errFrag) {
					t.Fatalf("error %q does not mention %q", err, tc.errFrag)
				}
				return
			}
			if err != nil {
				t.Fatalf("AudienceFromCluster: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("found = %v, want %v", ok, tc.wantOK)
			}
			if audience != tc.want {
				t.Fatalf("audience = %q, want %q", audience, tc.want)
			}
		})
	}
}

func TestDiscoverReturnsNothingWhenAuthenticationIsNotDeployed(t *testing.T) {
	source, found, err := discover(context.Background(), fake.NewClientset())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if found || source != nil {
		t.Fatalf("discover returned found=%v source=%v for a cluster with no auth ConfigMap", found, source)
	}
}

// A caller that cannot tell "no authentication" from "a Source" holds a nil
// pointer whose Token method panics.
func TestDiscoverReportsNotFoundRatherThanANilSource(t *testing.T) {
	t.Setenv(EnvDisable, "true")

	// A nil rest.Config is safe only because the disable check comes first.
	source, found, err := Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if found {
		t.Fatal("Discover reported found with NKE_E2E_AUTH_DISABLE set")
	}
	if source != nil {
		t.Fatal("Discover returned a source with NKE_E2E_AUTH_DISABLE set")
	}
}

// This variable turns the suite's credential off, and a suite with no
// credential goes green against an NKE that is not enforcing. ParseBool is the
// contract, so "no" and "yes" are errors, not guesses.
func TestDisabledParsesBooleans(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		// disabled() trims and hands the rest to strconv.ParseBool, so the whole
		// accept-set is ParseBool's to test. What belongs here: unset reads as
		// false, and the value is trimmed before parsing.
		{raw: "", want: false},
		{raw: "true", want: true},
		{raw: " true ", want: true},
		{raw: "false", want: false},
		{raw: "no", wantErr: true},
	} {
		t.Run(strconv.Quote(tc.raw), func(t *testing.T) {
			t.Setenv(EnvDisable, tc.raw)
			got, err := disabled()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("disabled() = %v with %s=%q, want an error", got, EnvDisable, tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("disabled(): %v", err)
			}
			if got != tc.want {
				t.Fatalf("disabled() = %v with %s=%q, want %v", got, EnvDisable, tc.raw, tc.want)
			}
		})
	}
}

// An unparseable value stops Discover, because either guess is wrong in a way
// nobody would notice.
func TestDiscoverRejectsAnUnparseableDisable(t *testing.T) {
	t.Setenv(EnvDisable, "maybe")
	if _, _, err := Discover(context.Background(), nil); err == nil {
		t.Fatalf("Discover succeeded with %s=maybe, want an error", EnvDisable)
	}
}

func TestDiscoverHonoursTheEnvironmentOverrides(t *testing.T) {
	t.Setenv(EnvAudience, "https://nke.wrong.invalid")
	t.Setenv(EnvNamespace, "nke-outsiders")
	t.Setenv(EnvServiceAccount, "nke-outsider")
	t.Setenv(EnvTTL, "20m")

	// No ConfigMap: the override alone has to be enough, so a caller without
	// read access to the NKE namespace can still run the suite.
	source, found, err := discover(context.Background(), fake.NewClientset())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !found || source == nil {
		t.Fatal("discover returned no source despite an explicit audience")
	}
	if source.Audience() != "https://nke.wrong.invalid" {
		t.Errorf("audience = %q, want the override", source.Audience())
	}
	if source.Identity() != "nke-outsiders/nke-outsider" {
		t.Errorf("identity = %q, want nke-outsiders/nke-outsider", source.Identity())
	}
	if source.ttl != 20*time.Minute {
		t.Errorf("ttl = %s, want 20m", source.ttl)
	}
}

func TestDiscoverRejectsAnUnparseableTTL(t *testing.T) {
	t.Setenv(EnvAudience, "https://nke.tilt.local")
	t.Setenv(EnvTTL, "twenty minutes")

	if _, _, err := discover(context.Background(), fake.NewClientset()); err == nil {
		t.Fatal("discover succeeded with an unparseable TTL, want an error")
	}
}
