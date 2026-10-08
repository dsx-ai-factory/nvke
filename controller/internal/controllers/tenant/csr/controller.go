// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package csr

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenant"
	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
)

const (
	decisionApprove = "approve"
	decisionSkip    = "skip"

	nodeUsernamePrefix = "system:node:"
	nodeGroup          = "system:nodes"

	approvalReason = "NKEValidatedNodeIdentity"

	unsupportedSANReason = "subject alternative names contain an unsupported identity"
)

type database interface {
	GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error)
	ListNodes(ctx context.Context, filters db.NodeFilters) ([]db.Node, error)
}

// Reconciler approves pending kubelet-serving CSRs that match the node identity
// recorded in the controller.
type Reconciler struct {
	clusterID uuid.UUID
	database  database
	client    client.Client
	logger    *slog.Logger
	audit     *credentialaudit.Recorder
}

type csrDecision struct {
	CSR      *certificatesv1.CertificateSigningRequest
	NodeName string
	Action   string
	Reason   string
}

// NewRegistrar returns a tenant-manager registrar. Each tenant manager
// gets its own CSR informer and work queue; clusterID is therefore implicit in
// every queued CSR name and cannot be confused with another tenant cluster.
func NewRegistrar(database database, logger *slog.Logger) tenant.Registrar {
	if logger == nil {
		logger = slog.Default()
	}
	var audit *credentialaudit.Recorder
	if store, ok := database.(credentialaudit.Store); ok {
		audit = credentialaudit.NewRecorder(store, logger, metrics.RecordCredentialAuditWrite)
	}
	return func(clusterID uuid.UUID, manager ctrl.Manager) error {
		controllerName, controllerOptions := tenantCSRControllerRegistration(clusterID)
		reconciler := &Reconciler{
			clusterID: clusterID,
			database:  database,
			client:    manager.GetClient(),
			logger:    logger,
			audit:     audit,
		}
		return ctrl.NewControllerManagedBy(manager).
			Named(controllerName).
			WithOptions(controllerOptions).
			For(
				&certificatesv1.CertificateSigningRequest{},
				builder.WithPredicates(kubeletServingCSRPredicate()),
			).
			Complete(reconciler)
	}
}

func tenantCSRControllerRegistration(clusterID uuid.UUID) (string, controller.Options) {
	// controller-runtime retains controller names for the lifetime of the
	// process, even after their manager stops. ManagerSet serializes manager
	// generations for a cluster, so the same name is never active concurrently;
	// skipping validation permits kubeconfig rotation and runtime recovery while
	// the cluster-scoped name keeps metrics and logs attributable.
	return "tenant-csr-" + clusterID.String(), controller.Options{
		SkipNameValidation: ptr.To(true),
	}
}

func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	csr := &certificatesv1.CertificateSigningRequest{}
	if err := r.client.Get(ctx, request.NamespacedName, csr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting tenant CSR %s for cluster %s: %w", request.Name, r.clusterID, err)
	}
	if !isPendingKubeletServingCSR(csr) {
		r.auditIssuedCertificate(ctx, csr)
		return ctrl.Result{}, nil
	}
	cluster, err := r.database.GetCluster(ctx, r.clusterID)
	if errors.Is(err, db.ErrNotFound) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting controller cluster %s: %w", r.clusterID, err)
	}
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return ctrl.Result{}, nil
	}

	nodes, err := r.database.ListNodes(ctx, db.NodeFilters{ClusterID: &r.clusterID})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing controller nodes for cluster %s: %w", r.clusterID, err)
	}
	decision := planDecision(csr, nodes)
	if decision.Action == decisionSkip {
		// Only a managed node's rejected CSR is ledger evidence. Other requestors
		// are tenant-controlled and would otherwise add unbounded rows.
		if _, managed := knownNodeIP(nodes, decision.NodeName); managed {
			r.auditDecision(ctx, decision, nodes, credentialaudit.ActionCheck, credentialaudit.OutcomeNoop, credentialaudit.ReasonRejected)
		}
		r.logDecision(decision)
		return ctrl.Result{}, nil
	}
	r.auditDecision(ctx, decision, nodes, credentialaudit.ActionCheck, credentialaudit.OutcomeSuccess, credentialaudit.ReasonVerified)
	if err := r.applyDecision(ctx, decision); err != nil {
		r.auditDecision(ctx, decision, nodes, credentialaudit.ActionApprove, credentialaudit.OutcomeFailure, credentialaudit.ReasonApprovalFail)
		return ctrl.Result{}, err
	}
	r.auditDecision(ctx, decision, nodes, credentialaudit.ActionApprove, credentialaudit.OutcomeSuccess, credentialaudit.ReasonCompleted)
	r.logDecision(decision)
	return ctrl.Result{}, nil
}

func (r *Reconciler) auditDecision(ctx context.Context, decision csrDecision, nodes []db.Node, action, outcome, reason string) {
	if r.audit == nil {
		return
	}
	r.audit.Record(ctx, r.csrAuditEvent(decision, nodes, action, outcome, reason))
}

func (r *Reconciler) csrAuditEvent(decision csrDecision, nodes []db.Node, action, outcome, reason string) credentialaudit.Event {
	target := credentialaudit.Target{ClusterID: r.clusterID.String()}
	for _, node := range nodes {
		if node.Name == decision.NodeName {
			target.NodeID = node.ID.String()
			break
		}
	}
	return credentialaudit.Event{
		Timestamp: time.Now().UTC(), Action: action, Credential: credentialaudit.TypeServing,
		Plane: credentialaudit.PlaneT2, Target: target, Actor: credentialaudit.Actor{Kind: credentialaudit.ActorAutomatic},
		Outcome: outcome, Reason: reason,
		Correlation: credentialaudit.Correlation{OperationHash: credentialaudit.Hash("operation",
			string(decision.CSR.UID))},
	}
}

// auditIssuedCertificate observes the public signer result without claiming the
// kubelet has installed it. The CSR must still match a managed node's identity.
func (r *Reconciler) auditIssuedCertificate(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) {
	if r.audit == nil || !hasIssuedServingCertificate(csr) {
		return
	}
	cluster, err := r.database.GetCluster(ctx, r.clusterID)
	if err != nil || cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return
	}
	nodes, err := r.database.ListNodes(ctx, db.NodeFilters{ClusterID: &r.clusterID})
	if err != nil {
		return
	}
	decision := planDecision(csr, nodes)
	if decision.Action != decisionApprove {
		return
	}
	event := r.csrAuditEvent(decision, nodes, credentialaudit.ActionVerify, credentialaudit.OutcomeSuccess, credentialaudit.ReasonObserved)
	fingerprints, err := credentialaudit.CertificateFingerprints(csr.Status.Certificate)
	if err != nil {
		event.Outcome, event.Reason = credentialaudit.OutcomeFailure, credentialaudit.ReasonUnavailable
	} else {
		event.Fingerprints = fingerprints
	}
	r.audit.Record(ctx, event)
}

func hasIssuedServingCertificate(csr *certificatesv1.CertificateSigningRequest) bool {
	return csr.Spec.SignerName == certificatesv1.KubeletServingSignerName && len(csr.Status.Certificate) > 0
}

func (r *Reconciler) applyDecision(ctx context.Context, decision csrDecision) error {
	now := metav1.Now()
	decision.CSR.Status.Conditions = append(decision.CSR.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:               certificatesv1.CertificateApproved,
		Status:             corev1.ConditionTrue,
		Reason:             approvalReason,
		Message:            "kubelet-serving CSR matches the node identity recorded by NVKE",
		LastUpdateTime:     now,
		LastTransitionTime: now,
	})
	if err := r.client.SubResource("approval").Update(ctx, decision.CSR); err != nil {
		return fmt.Errorf(
			"applying %s decision to tenant CSR %s for cluster %s: %w",
			decision.Action,
			decision.CSR.Name,
			r.clusterID,
			err,
		)
	}
	return nil
}

func (r *Reconciler) logDecision(decision csrDecision) {
	log := r.logger.Info
	if decision.Action == decisionSkip {
		log = r.logger.Debug
	}
	log(
		"Reconciled tenant kubelet-serving CSR",
		"clusterID", r.clusterID,
		"requestHash", credentialaudit.Hash("operation", string(decision.CSR.UID)),
		"nodeName", decision.NodeName,
		"decision", decision.Action,
		"reason", decision.Reason,
	)
}

func kubeletServingCSRPredicate() predicate.Predicate {
	matches := func(object client.Object) bool {
		csr, ok := object.(*certificatesv1.CertificateSigningRequest)
		return ok && (isPendingKubeletServingCSR(csr) || hasIssuedServingCertificate(csr))
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return matches(e.Object) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return matches(e.ObjectOld) || matches(e.ObjectNew) },
		GenericFunc: func(e event.GenericEvent) bool { return matches(e.Object) },
	}
}

func isPendingKubeletServingCSR(csr *certificatesv1.CertificateSigningRequest) bool {
	if csr.Spec.SignerName != certificatesv1.KubeletServingSignerName {
		return false
	}
	for _, condition := range csr.Status.Conditions {
		switch condition.Type {
		case certificatesv1.CertificateApproved, certificatesv1.CertificateDenied, certificatesv1.CertificateFailed:
			return false
		}
	}
	return true
}

func planDecision(csr *certificatesv1.CertificateSigningRequest, nodes []db.Node) csrDecision {
	nodeName, ok := strings.CutPrefix(csr.Spec.Username, nodeUsernamePrefix)
	if !ok || nodeName == "" {
		return newDecision(csr, "", decisionSkip, "requestor is not a system:node identity")
	}

	expectedIP, known := knownNodeIP(nodes, nodeName)
	if !known {
		return newDecision(csr, nodeName, decisionSkip, "node is not present in the controller's desired state")
	}
	if reason := validateServingCSR(csr, nodeName, expectedIP); reason != "" {
		return newDecision(csr, nodeName, decisionSkip, reason)
	}
	return newDecision(csr, nodeName, decisionApprove, "matches controller node identity")
}

func newDecision(csr *certificatesv1.CertificateSigningRequest, nodeName, action, reason string) csrDecision {
	return csrDecision{
		CSR:      csr.DeepCopy(),
		NodeName: nodeName,
		Action:   action,
		Reason:   reason,
	}
}

func knownNodeIP(nodes []db.Node, nodeName string) (string, bool) {
	for _, node := range nodes {
		if node.Name == nodeName {
			return node.IP, true
		}
	}
	return "", false
}

// validateServingCSR returns an empty string when a CSR matches the identity
// recorded in the controller. A non-empty return value explains why it must not be
// approved.
func validateServingCSR(csr *certificatesv1.CertificateSigningRequest, nodeName, expectedIP string) string {
	if !slices.Contains(csr.Spec.Groups, nodeGroup) {
		return "spec.groups does not include system:nodes"
	}

	hasServerAuth := false
	for _, usage := range csr.Spec.Usages {
		if usage == certificatesv1.UsageServerAuth {
			hasServerAuth = true
		}
		if usage != certificatesv1.UsageServerAuth &&
			usage != certificatesv1.UsageDigitalSignature &&
			usage != certificatesv1.UsageKeyEncipherment {
			return "spec.usages contains a disallowed usage"
		}
	}
	if !hasServerAuth {
		return "spec.usages does not include server auth"
	}

	pemBlock, trailing := pem.Decode(csr.Spec.Request)
	if pemBlock == nil || strings.TrimSpace(string(trailing)) != "" {
		return "spec.request is not a single PEM-encoded certificate request"
	}
	parsed, err := x509.ParseCertificateRequest(pemBlock.Bytes)
	if err != nil {
		return "spec.request cannot be parsed as a certificate request"
	}
	if err := parsed.CheckSignature(); err != nil {
		return "certificate request signature is invalid"
	}
	if !slices.Contains(parsed.Subject.Organization, nodeGroup) {
		return "subject organization is not system:nodes"
	}
	expectedCommonName := nodeUsernamePrefix + nodeName
	if parsed.Subject.CommonName != expectedCommonName {
		return fmt.Sprintf("subject common name is not %s", expectedCommonName)
	}
	if reason := validateSubjectAlternativeNames(parsed, nodeName, expectedIP); reason != "" {
		return reason
	}
	return ""
}

func validateSubjectAlternativeNames(csr *x509.CertificateRequest, nodeName, expectedIP string) string {
	if len(csr.EmailAddresses) > 0 || len(csr.URIs) > 0 {
		return unsupportedSANReason
	}
	if len(csr.DNSNames) == 0 && len(csr.IPAddresses) == 0 {
		return "certificate request has no subject alternative names"
	}

	for _, dnsName := range csr.DNSNames {
		if dnsName != nodeName {
			return "DNS SAN does not match controller node name"
		}
	}

	if len(csr.IPAddresses) == 0 {
		return ""
	}
	expected := net.ParseIP(expectedIP)
	for _, ipAddress := range csr.IPAddresses {
		if expected == nil || !ipAddress.Equal(expected) {
			return "IP SAN does not match controller node IP"
		}
	}
	return ""
}
