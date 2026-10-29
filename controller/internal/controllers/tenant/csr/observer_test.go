// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package csr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestServingCSRSnapshotCountsPendingIssuanceAndTerminalFailures(t *testing.T) {
	now := time.Now().UTC()
	created := now.Add(-time.Hour)
	makeCSR := func(condition certificatesv1.RequestConditionType, issued bool) certificatesv1.CertificateSigningRequest {
		item := certificatesv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)}, Spec: certificatesv1.CertificateSigningRequestSpec{SignerName: certificatesv1.KubeletServingSignerName}}
		if condition != "" {
			item.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{Type: condition, Status: corev1.ConditionTrue}}
		}
		if issued {
			item.Status.Certificate = []byte("issued")
		}
		return item
	}
	unrelated := makeCSR("", false)
	unrelated.Spec.SignerName = certificatesv1.KubeAPIServerClientSignerName
	items := []certificatesv1.CertificateSigningRequest{makeCSR("", false), makeCSR(certificatesv1.CertificateApproved, false), makeCSR(certificatesv1.CertificateApproved, true), makeCSR(certificatesv1.CertificateFailed, false), makeCSR(certificatesv1.CertificateDenied, false), unrelated}
	got := summarizeServingCSRs(items, now)
	require.Equal(t, 2, got.Pending)
	require.Equal(t, 1, got.Failed)
	require.Equal(t, 1, got.Denied)
	require.Equal(t, &created, got.OldestPendingAt)
	deleted := summarizeServingCSRs(nil, now.Add(time.Minute))
	require.Zero(t, deleted.Pending)
	require.Zero(t, deleted.Failed)
	require.Nil(t, deleted.OldestPendingAt)
}

func TestObserverPreservesLastAPIObservationAfterCacheDisconnect(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	pending := certificatesv1.CertificateSigningRequest{
		TypeMeta:   metav1.TypeMeta{APIVersion: certificatesv1.SchemeGroupVersion.String(), Kind: "CertificateSigningRequest"},
		ObjectMeta: metav1.ObjectMeta{Name: "pending-serving", ResourceVersion: "1", CreationTimestamp: metav1.NewTime(created)},
		Spec:       certificatesv1.CertificateSigningRequestSpec{SignerName: certificatesv1.KubeletServingSignerName},
	}
	failed := *pending.DeepCopy()
	failed.Name = "failed-serving"
	failed.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{Type: certificatesv1.CertificateFailed, Status: corev1.ConditionTrue}}
	for _, test := range []struct {
		name    string
		items   []certificatesv1.CertificateSigningRequest
		pending int
		failed  int
	}{
		{name: "empty inventory", items: []certificatesv1.CertificateSigningRequest{}},
		{name: "pending and failed inventory", items: []certificatesv1.CertificateSigningRequest{pending, failed}, pending: 1, failed: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			server, cached, live := syncedCSRReaders(t, ctx, test.items)
			database := csrObserverSQLiteDB{DB: db.NewDB(testutil.SetupDB(ctx, t, (*db.Cluster)(nil)))}
			cluster, err := database.CreateCluster(ctx, &db.Cluster{ID: uuid.New(), Name: "observed-cluster"})
			require.NoError(t, err)
			mgr := &csrObserverManager{cached: cached, live: live}
			require.NoError(t, NewObserverRegistrar(dbwrapper.New(database, slog.New(slog.DiscardHandler)), slog.New(slog.DiscardHandler))(cluster.ID, mgr))
			observer, ok := mgr.runnable.(*credentialObserver)
			require.True(t, ok)
			require.NoError(t, observer.observe(ctx))
			before, err := database.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			require.NotNil(t, before.Conditions.CredentialCSRs)
			require.Equal(t, test.pending, before.Conditions.CredentialCSRs.Pending)
			require.Equal(t, test.failed, before.Conditions.CredentialCSRs.Failed)
			if test.pending > 0 {
				// The database returns the instant in the local zone, so compare instants.
				require.NotNil(t, before.Conditions.CredentialCSRs.OldestPendingAt)
				require.True(t, created.Equal(*before.Conditions.CredentialCSRs.OldestPendingAt))
			}

			// The real controller-runtime cache keeps answering after its API and watch close.
			server.CloseClientConnections()
			server.Close()
			stale := &certificatesv1.CertificateSigningRequestList{}
			require.NoError(t, cached.List(ctx, stale))
			require.Len(t, stale.Items, len(test.items))
			for range 2 {
				err = observer.observe(ctx)
				require.ErrorContains(t, err, "list serving CSRs")
				after, err := database.GetCluster(ctx, cluster.ID)
				require.NoError(t, err)
				// Counts, oldest pending time and successful observation time all survive.
				require.Equal(t, before.Conditions.CredentialCSRs, after.Conditions.CredentialCSRs)
			}
		})
	}
}

func TestObserverWritesOnlyChangedSnapshotsOrHeartbeats(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	pending := certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-serving", CreationTimestamp: metav1.NewTime(created)},
		Spec:       certificatesv1.CertificateSigningRequestSpec{SignerName: certificatesv1.KubeletServingSignerName},
	}
	for _, test := range []struct {
		name      string
		storedAge time.Duration
		items     []certificatesv1.CertificateSigningRequest
		wantWrite bool
	}{
		{name: "unchanged and recent", storedAge: observationHeartbeat / 2, items: []certificatesv1.CertificateSigningRequest{pending}},
		{name: "unchanged but past the heartbeat", storedAge: 2 * observationHeartbeat, items: []certificatesv1.CertificateSigningRequest{pending}, wantWrite: true},
		{name: "changed inventory", storedAge: time.Second, items: nil, wantWrite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			database := csrObserverSQLiteDB{DB: db.NewDB(testutil.SetupDB(ctx, t, (*db.Cluster)(nil)))}
			stored := db.CredentialCSRObservation{ObservedAt: time.Now().UTC().Add(-test.storedAge), Pending: 1, OldestPendingAt: &created}
			cluster, err := database.CreateCluster(ctx, &db.Cluster{ID: uuid.New(), Name: "observed-cluster", Conditions: db.ClusterConditions{CredentialCSRs: &stored}})
			require.NoError(t, err)
			before, err := database.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			observer := &credentialObserver{clusterID: cluster.ID, database: database, reader: &csrListReader{items: test.items}, logger: slog.New(slog.DiscardHandler)}

			require.NoError(t, observer.observe(ctx))

			after, err := database.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			// A snapshot write is not a cluster change, so updated_at never moves.
			require.True(t, after.UpdatedAt.Equal(before.UpdatedAt), "a snapshot write changed updated_at")
			if test.wantWrite {
				require.True(t, after.Conditions.CredentialCSRs.ObservedAt.After(before.Conditions.CredentialCSRs.ObservedAt))
				return
			}
			require.Equal(t, before.Conditions.CredentialCSRs, after.Conditions.CredentialCSRs)
		})
	}
}

func TestObserverWriteDoesNotQueueClusterReconciles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sqlite := csrObserverSQLiteDB{DB: db.NewDB(testutil.SetupDB(ctx, t, (*db.Cluster)(nil)))}
	wrapper := dbwrapper.New(sqlite, slog.New(slog.DiscardHandler))
	// Started before the cluster exists, so the source seeds nothing.
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	require.NoError(t, dbwrapper.NewClusterSource(wrapper, dbfilters.Cluster{}).Start(ctx, queue))
	cluster, err := sqlite.CreateCluster(ctx, &db.Cluster{ID: uuid.New(), Name: "observed-cluster"})
	require.NoError(t, err)

	mgr := &csrObserverManager{live: &csrListReader{}}
	require.NoError(t, NewObserverRegistrar(wrapper, slog.New(slog.DiscardHandler))(cluster.ID, mgr))
	observer, ok := mgr.runnable.(*credentialObserver)
	require.True(t, ok)
	require.NoError(t, observer.observe(ctx))

	stored, err := sqlite.GetCluster(ctx, cluster.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Conditions.CredentialCSRs, "the observer did not write")
	require.Never(t, func() bool { return queue.Len() > 0 }, 300*time.Millisecond, 10*time.Millisecond)

	// The wrapper does queue a reconcile for an ordinary write, so the check above can fail.
	_, err = wrapper.UpdateCluster(ctx, &stored)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return queue.Len() > 0 }, 5*time.Second, 10*time.Millisecond)
}

func TestObserverStartBoundsAPIReadsAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &csrBlockingReader{started: make(chan context.Context, 1)}
	observer := &credentialObserver{reader: reader, logger: slog.New(slog.DiscardHandler)}
	done := make(chan error, 1)
	go func() { done <- observer.Start(ctx) }()
	select {
	case probeCtx := <-reader.started:
		deadline, bounded := probeCtx.Deadline()
		require.True(t, bounded)
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not start its API read")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not stop its API read")
	}
}

type csrBlockingReader struct {
	client.Reader
	started chan context.Context
}

func (r *csrBlockingReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	r.started <- ctx
	<-ctx.Done()
	return ctx.Err()
}

type csrListReader struct {
	client.Reader
	items []certificatesv1.CertificateSigningRequest
}

func (r *csrListReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	list.(*certificatesv1.CertificateSigningRequestList).Items = r.items
	return nil
}

func syncedCSRReaders(t *testing.T, ctx context.Context, items []certificatesv1.CertificateSigningRequest) (*httptest.Server, client.Client, client.Reader) {
	t.Helper()
	list := certificatesv1.CertificateSigningRequestList{
		TypeMeta: metav1.TypeMeta{APIVersion: certificatesv1.SchemeGroupVersion.String(), Kind: "CertificateSigningRequestList"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    items,
	}
	listJSON, err := json.Marshal(list)
	require.NoError(t, err)
	var initialEvents []byte
	for _, item := range items {
		itemJSON, err := json.Marshal(item)
		require.NoError(t, err)
		eventJSON, err := json.Marshal(metav1.WatchEvent{Type: "ADDED", Object: runtime.RawExtension{Raw: itemJSON}})
		require.NoError(t, err)
		initialEvents = append(initialEvents, append(eventJSON, '\n')...)
	}
	bookmark := `{"type":"BOOKMARK","object":{"apiVersion":"certificates.k8s.io/v1","kind":"CertificateSigningRequest","metadata":{"resourceVersion":"1","annotations":{"k8s.io/initial-events-end":"true"}}}}` + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			if r.URL.Query().Get("sendInitialEvents") == "true" {
				_, _ = w.Write(initialEvents)
				_, _ = w.Write([]byte(bookmark))
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(listJSON)
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{certificatesv1.SchemeGroupVersion})
	mapper.Add(certificatesv1.SchemeGroupVersion.WithKind("CertificateSigningRequest"), meta.RESTScopeRoot)
	config := &rest.Config{Host: server.URL}
	localCache, err := cache.New(config, cache.Options{Scheme: scheme, Mapper: mapper})
	require.NoError(t, err)
	_, err = localCache.GetInformer(ctx, &certificatesv1.CertificateSigningRequest{}, cache.BlockUntilSynced(false))
	require.NoError(t, err)
	cacheCtx, stopCache := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- localCache.Start(cacheCtx) }()
	t.Cleanup(func() {
		stopCache()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("CSR cache did not stop")
		}
	})
	require.True(t, localCache.WaitForCacheSync(ctx))
	cached, err := client.New(config, client.Options{Scheme: scheme, Mapper: mapper, Cache: &client.CacheOptions{Reader: localCache}})
	require.NoError(t, err)
	live, err := client.New(config, client.Options{Scheme: scheme, Mapper: mapper})
	require.NoError(t, err)
	return server, cached, live
}

type csrObserverManager struct {
	manager.Manager
	cached   client.Client
	live     client.Reader
	runnable manager.Runnable
}

func (m *csrObserverManager) GetClient() client.Client { return m.cached }

func (m *csrObserverManager) GetAPIReader() client.Reader { return m.live }

func (m *csrObserverManager) Add(runnable manager.Runnable) error {
	m.runnable = runnable
	return nil
}

// SQLite supports the real JSON transaction path but does not implement FOR UPDATE.
type csrObserverSQLiteDB struct{ db.DB }

func (d csrObserverSQLiteDB) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return d.DB.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error { return fn(ctx, csrObserverSQLiteTx{Tx: tx}) })
}

type csrObserverSQLiteTx struct{ db.Tx }

func (t csrObserverSQLiteTx) GetClusterForUpdate(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	return t.Tx.GetCluster(ctx, id)
}
