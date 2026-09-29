// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package forge provisions the machine a site controller is built on, on
// Forge, through the ncx-infra-controller-rest SDK.
package forge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
	"golang.org/x/crypto/ssh"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/environment"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra/teleport"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
)

const (
	// DefaultEndpoint is production NGC. Staging is
	// https://api.stg.ngc.nvidia.com.
	DefaultEndpoint = "https://api.ngc.nvidia.com"

	// apiName is the path segment Forge serves under, where the SDK's
	// generated paths say carbide.
	apiName = "forge"

	defaultReadyTimeout = 30 * time.Minute
	defaultReachTimeout = 20 * time.Minute
	defaultPollInterval = 30 * time.Second

	// releaseTimeout bounds the delete issued when provisioning fails
	// part-way. It runs detached from the caller's context, so it needs a
	// bound of its own.
	releaseTimeout = 5 * time.Minute

	// defaultCreateTimeout bounds the create call alone, and is deliberately
	// far longer than the timeout the polls run under. Losing the response to
	// a create is the one unrecoverable failure here: the id it carries is
	// the only handle on the instance, and Forge has built the machine
	// whether or not the answer arrives.
	defaultCreateTimeout = 10 * time.Minute

	// createMayHaveLanded is appended to every create failure. Nothing in
	// this process can clean up after one -- the list endpoint caps at twenty
	// rows and ignores every filter, so an instance whose id was never
	// returned cannot be found again. Say so rather than report a tidy
	// failure over a machine that is still running and still billing.
	createMayHaveLanded = "\nthe instance may exist even so: Forge builds it before it answers, and without the id in that answer nothing here can delete it -- check the Forge console for this name"
)

// Images resolves the Node version under qualification to the two URLs the
// iPXE script boots: the qcow imager and the image it writes. Both must be
// plain HTTPS, since iPXE cannot pull an OCI artifact.
//
// The architecture is a parameter because a run can span two: the machine
// being created decides which image it needs, and only the provider knows
// which machine that is.
type Images func(nodeVersion, arch string) (bootLoaderURL, imageURL string, err error)

// Conn is a connection to a node. It is everything a Session does except
// release the machine, which is the provider's.
type Conn interface {
	Run(ctx context.Context, cmd string) (string, error)
	Put(ctx context.Context, path string, data []byte, mode uint32) error
	Fetch(ctx context.Context, path string) ([]byte, error)
	Forward(ctx context.Context, remote string) (string, error)
	Close() error
}

// Dialer opens a connection to a node through its site's jump host. A dial
// that fails because the node is not answering yet is retried by the
// provider, so a Dialer makes one attempt and reports what happened.
type Dialer interface {
	Dial(ctx context.Context, target teleport.Target) (Conn, error)
}

// Teleport is the Dialer a real run uses.
func Teleport(d *teleport.Dialer) Dialer { return teleportDialer{d} }

type teleportDialer struct{ d *teleport.Dialer }

func (t teleportDialer) Dial(ctx context.Context, target teleport.Target) (Conn, error) {
	c, err := t.d.Dial(ctx, target)
	if err != nil {
		// Not c: a nil *Client in a Conn is a non-nil Conn.
		return nil, err
	}
	return c, nil
}

// Options configures a Provider.
type Options struct {
	Env  *environment.Forge
	Site string // empty resolves to the only site declared

	// APIKey is an NGC key. An nvapi- key is the bearer token as-is; a legacy
	// key is exchanged for one.
	APIKey   string
	Endpoint string // empty is DefaultEndpoint
	AuthURL  string // empty pairs the token service with Endpoint

	Images Images
	Dialer Dialer
	// NodeUser is the account the OS image bakes, and the login every
	// session uses. Empty is the OSS os-image default; an internal-pipeline
	// image needs its own. It must match the image or cloud-init writes
	// authorized_keys for an account that does not exist.
	NodeUser string

	// JumpHost is the Teleport node the site's machines are reached through.
	// They are not Teleport-enrolled themselves, so it is the only route to
	// one. It comes from the runner, not the environment file, so the access
	// path is not published with the site.
	JumpHost string

	// Node carries the package versions under qualification. Its image URLs
	// are filled from Images, so any set here are ignored.
	Node  nke.NodeInputs
	Tools HostTools

	// DisableMTLS runs the node agent without SPIFFE. nkectl bootstrap site
	// must be given --disable-mtls to match, and the stack deployed with
	// spiffe.enabled=false, or every dial fails as a TLS handshake error.
	DisableMTLS bool

	HTTPClient   *http.Client
	ReadyTimeout time.Duration
	ReachTimeout time.Duration
	PollInterval time.Duration

	// CreateTimeout bounds the create call, separately from HTTPClient's
	// timeout, which bounds everything else. See defaultCreateTimeout.
	CreateTimeout time.Duration
}

// Provider creates Forge instances ready for `nkectl bootstrap site`.
type Provider struct {
	opts Options
	site environment.ForgeSite
	// client serves everything but create. Its short per-request timeout is
	// what makes a hung poll fail and retry rather than stall.
	client *standard.APIClient
	// createClient serves create alone, under a far longer timeout, because
	// that call's response cannot be given up on. See defaultCreateTimeout.
	createClient *standard.APIClient
	token        *tokenSource
}

var (
	_ infra.Provider = (*Provider)(nil)
	_ infra.Session  = (*session)(nil)
)

// New validates opts against the environment and returns a Provider. It
// makes no API call, so a misconfigured run fails before anything is created.
func New(opts Options) (*Provider, error) {
	if opts.Env == nil {
		return nil, errors.New("forge: no environment")
	}
	site, err := opts.Env.Site(opts.Site)
	if err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	if strings.TrimSpace(opts.JumpHost) == "" {
		return nil, fmt.Errorf("forge: no jump host for site %q, and its machines are reachable only through one", site.Key)
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("forge: no API key")
	}
	if opts.Images == nil {
		return nil, errors.New("forge: no image resolver")
	}
	if opts.Dialer == nil {
		return nil, errors.New("forge: no dialer")
	}
	if err := opts.Tools.validate(); err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultEndpoint
	}
	if opts.AuthURL == "" {
		opts.AuthURL = defaultAuthURL(opts.Endpoint)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: time.Minute}
	}
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = defaultReadyTimeout
	}
	if opts.ReachTimeout == 0 {
		opts.ReachTimeout = defaultReachTimeout
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.CreateTimeout == 0 {
		opts.CreateTimeout = defaultCreateTimeout
	}

	endpoint := strings.TrimRight(opts.Endpoint, "/")
	// Taken before either SetAPIName below, which wraps the transport of the
	// client it is given: a copy made afterwards would be wrapped twice.
	createHTTP := withTimeout(opts.HTTPClient, opts.CreateTimeout)

	cfg := standard.NewConfiguration()
	cfg.Servers = standard.ServerConfigurations{{URL: endpoint}}
	cfg.HTTPClient = opts.HTTPClient
	// After HTTPClient, because it wraps that client's transport.
	cfg.SetAPIName(apiName)

	// Its own Servers value rather than a shared slice, so neither
	// configuration can be reached through the other's.
	createCfg := standard.NewConfiguration()
	createCfg.Servers = standard.ServerConfigurations{{URL: endpoint}}
	createCfg.HTTPClient = createHTTP
	createCfg.SetAPIName(apiName)

	return &Provider{
		opts:         opts,
		site:         site,
		client:       standard.NewAPIClient(cfg),
		createClient: standard.NewAPIClient(createCfg),
		token:        newTokenSource(opts.HTTPClient, opts.APIKey, opts.Env.Org.ID, opts.AuthURL),
	}, nil
}

// withTimeout copies c with at least the given timeout, raising a shorter one
// and leaving an absent one absent. A copy rather than a mutation: the caller
// keeps its client, and a test that injected a transport keeps that too.
func withTimeout(c *http.Client, timeout time.Duration) *http.Client {
	copied := *c
	if copied.Timeout != 0 && copied.Timeout < timeout {
		copied.Timeout = timeout
	}
	return &copied
}

// ProvisionNode creates one instance and returns once a session to it can run
// commands and cloud-init has finished. An instance created and then not
// brought that far is deleted before the error is returned, so a failed run
// does not leak hardware.
func (p *Provider) ProvisionNode(ctx context.Context, req infra.NodeRequest) (infra.Node, infra.Session, error) {
	if strings.TrimSpace(req.Name) == "" {
		return infra.Node{}, nil, errors.New("forge: node request has no name")
	}
	instanceType, err := p.site.MatchInstanceType(req.Capabilities)
	if err != nil {
		return infra.Node{}, nil, fmt.Errorf("forge: %w", err)
	}

	signer, authorizedKey, err := newKey()
	if err != nil {
		return infra.Node{}, nil, err
	}
	ipxe, userData, err := p.payloads(req, instanceType, authorizedKey)
	if err != nil {
		return infra.Node{}, nil, err
	}

	ctx, err = p.authorize(ctx)
	if err != nil {
		return infra.Node{}, nil, err
	}
	tenantID, err := p.tenantID(ctx)
	if err != nil {
		return infra.Node{}, nil, err
	}

	create := standard.NewInstanceCreateRequest(req.Name, tenantID, p.site.VpcID, p.interfaces())
	create.SetInstanceTypeId(instanceType.ID)
	create.SetIpxeScript(ipxe)
	create.SetUserData(userData)
	create.SetAlwaysBootWithCustomIpxe(true)
	// A workaround, not a choice. Phone-home is what flips an instance from
	// Provisioning to Ready, and since 2026-09-19 new instances cannot reach
	// the metadata endpoint, so the callback never completes. With it off,
	// Ready no longer means cloud-init finished, which is why the node is
	// held until cloud-init reports done over the session. Flip back when
	// Forge restores the metadata grant.
	create.SetPhoneHomeEnabled(false)
	create.SetSshKeyGroupIds([]string{p.site.SSHKeyGroupID})
	if len(req.Labels) > 0 {
		create.SetLabels(req.Labels)
	}

	inst, resp, err := p.createClient.InstanceAPI.CreateInstance(ctx, p.opts.Env.Org.ID).InstanceCreateRequest(*create).Execute()
	closeBody(resp)
	if err != nil {
		return infra.Node{}, nil, fmt.Errorf("forge: creating instance %s: %w%s", req.Name, apiError(err), createMayHaveLanded)
	}
	if inst.GetId() == "" {
		return infra.Node{}, nil, fmt.Errorf("forge: creating instance %s returned no id%s", req.Name, createMayHaveLanded)
	}
	id := inst.GetId()

	release := func(ctx context.Context) error { return p.release(ctx, id) }
	fail := func(err error) (infra.Node, infra.Session, error) {
		// Detached, because a canceled run is exactly when this matters most
		// and inheriting the cancellation would abandon the instance.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if rerr := release(rctx); rerr != nil {
			return infra.Node{}, nil, errors.Join(err, fmt.Errorf("forge: instance %s (%s) was not deleted and must be removed by hand: %w", req.Name, id, rerr))
		}
		return infra.Node{}, nil, err
	}

	address, err := p.waitReady(ctx, id)
	if err != nil {
		return fail(err)
	}
	conn, err := p.waitReachable(ctx, teleport.Target{JumpHost: p.opts.JumpHost, Address: address, User: p.nodeUser(), Signer: signer})
	if err != nil {
		// A key this run generated moments ago is not rejected by chance.
		// sshd answering and refusing publickey means cloud-init wrote
		// authorized_keys somewhere else, which is what a node.user that
		// does not match the account the OS image bakes looks like. Say so
		// here: the alternative is a bare auth error after the whole
		// provisioning wait, with nothing naming the setting to change.
		if strings.Contains(err.Error(), "unable to authenticate") {
			err = fmt.Errorf("%w\n\tthe node answered but refused this run's key, so cloud-init likely wrote authorized_keys for a different account: check node.user (%q) against the account %s bakes", err, p.nodeUser(), req.Name)
		}
		return fail(err)
	}
	if out, err := conn.Run(ctx, "cloud-init status --wait"); err != nil {
		diag := cloudInitDiagnostics(ctx, conn)
		_ = conn.Close()
		return fail(fmt.Errorf("forge: cloud-init on %s did not finish cleanly, and node preflight would reject it: %w\n%s%s", req.Name, err, out, diag))
	}

	return infra.Node{Name: req.Name, IP: address, Arch: instanceType.Arch()}, &session{Conn: conn, release: release}, nil
}

// cloudInitDiagnosticTimeout bounds the diagnostic collection. The context
// that reached this point may already be at its deadline, and a node that has
// stopped answering must not turn a reported failure into a hang.
const cloudInitDiagnosticTimeout = 2 * time.Minute

// cloudInitDiagnostics reports why cloud-init degraded. `cloud-init status
// --wait` gives an exit code and the word "done", which is not enough to act
// on once the run has deleted the instance.
//
// Best effort throughout: a probe that fails contributes its error rather
// than replacing the failure being reported, since the caller already has the
// only fact that must survive.
func cloudInitDiagnostics(ctx context.Context, conn Conn) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudInitDiagnosticTimeout)
	defer cancel()

	probes := []struct{ label, cmd string }{
		// --long carries the recoverable errors that set the exit code.
		{"cloud-init status --long", "cloud-init status --long"},
		// Where a failing runcmd prints. The usual answer is here.
		{"cloud-init-output.log (tail)", "sudo -n tail -n 120 /var/log/cloud-init-output.log"},
		// cloud-init's own warnings, for a module that failed before runcmd.
		{"cloud-init.log (warnings)", "sudo -n grep -aE 'WARNING|ERROR|CRITICAL' /var/log/cloud-init.log | tail -n 40"},
	}

	var b strings.Builder
	for _, probe := range probes {
		out, err := conn.Run(ctx, probe.cmd)
		b.WriteString("\n--- " + probe.label + " ---\n")
		if err != nil {
			b.WriteString("collecting failed: " + err.Error() + "\n")
		}
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			b.WriteString(trimmed + "\n")
		}
	}
	return b.String()
}

// payloads returns the iPXE script and the cloud-init user data, in that
// order.
func (p *Provider) payloads(req infra.NodeRequest, instanceType environment.ForgeInstanceType, authorizedKey string) (string, string, error) {
	// Resolved first: it selects the image, the console and kernel parameters
	// the iPXE script sets, and the host tools. The instance type is the only
	// thing here that knows which machine is being created.
	arch := instanceType.Arch()
	if arch == "" {
		return "", "", fmt.Errorf("forge: instance type %s declares no arch, so neither boot parameters nor host tools can be chosen for it", instanceType.Name)
	}
	bootLoader, image, err := p.opts.Images(req.NodeVersion, arch)
	if err != nil {
		return "", "", fmt.Errorf("forge: resolving the %s image for node version %q: %w", arch, req.NodeVersion, err)
	}

	inputs := p.opts.Node
	inputs.BootLoaderURL, inputs.ImageURL = bootLoader, image
	inputs.Arch = arch
	if p.opts.DisableMTLS {
		inputs.AgentArgs = "--secure-server-port 15005 --disable-mtls"
	}
	node, err := nke.BuildForgeNodePayloads(inputs)
	if err != nil {
		return "", "", fmt.Errorf("forge: %w", err)
	}
	userData, err := renderUserData(userDataInputs{
		Hostname:      req.Name,
		AuthorizedKey: authorizedKey,
		Arch:          arch,
		NodeCommands:  node.PreKubeadmCommands,
		NodeUser:      p.nodeUser(),
		Tools:         p.opts.Tools,
	})
	if err != nil {
		return "", "", fmt.Errorf("forge: %w", err)
	}
	return node.IPXEScript, userData, nil
}

// interfaces attaches the instance to the site's network. A VPC exposes
// subnets or prefixes according to its network virtualization, never both,
// so the site declares whichever one its fabric uses and this follows it.
func (p *Provider) interfaces() []standard.InterfaceCreateRequest {
	iface := standard.NewInterfaceCreateRequest()
	if p.site.VpcPrefixID != "" {
		iface.SetVpcPrefixId(p.site.VpcPrefixID)
	} else {
		iface.SetSubnetId(p.site.SubnetID)
	}
	iface.SetIsPhysical(true)
	return []standard.InterfaceCreateRequest{*iface}
}

func (p *Provider) authorize(ctx context.Context) (context.Context, error) {
	token, err := p.token.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	return context.WithValue(ctx, standard.ContextAccessToken, token), nil
}

// tenantID is the org's tenant, which the create request names separately
// from the org in its path.
func (p *Provider) tenantID(ctx context.Context) (string, error) {
	tenant, resp, err := p.client.TenantAPI.GetCurrentTenant(ctx, p.opts.Env.Org.ID).Execute()
	closeBody(resp)
	if err != nil {
		return "", fmt.Errorf("forge: reading the current tenant: %w", apiError(err))
	}
	if tenant.GetId() == "" {
		return "", errors.New("forge: the current tenant has no id")
	}
	return tenant.GetId(), nil
}

// waitReady polls by id rather than listing: the list endpoint caps at twenty
// rows and ignores every filter, so a new instance is often not in it.
func (p *Provider) waitReady(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ReadyTimeout)
	defer cancel()

	var last standard.InstanceStatus
	for {
		// Authorized per poll: this loop outlives an exchanged token.
		actx, err := p.authorize(ctx)
		if err != nil {
			return "", err
		}
		inst, resp, err := p.client.InstanceAPI.GetInstance(actx, p.opts.Env.Org.ID, id).Execute()
		closeBody(resp)
		if err == nil {
			last = inst.GetStatus()
			switch last {
			case standard.INSTANCESTATUS_READY:
				// Ready can be reported before the interface has its
				// address; keep polling until it does.
				if address := instanceAddress(inst); address != "" {
					return address, nil
				}
			case standard.INSTANCESTATUS_ERROR, standard.INSTANCESTATUS_TERMINATING:
				return "", fmt.Errorf("forge: instance %s went to %s while provisioning", id, last)
			case standard.INSTANCESTATUS_PENDING,
				standard.INSTANCESTATUS_PROVISIONING,
				standard.INSTANCESTATUS_CONFIGURING,
				standard.INSTANCESTATUS_UPDATING,
				standard.INSTANCESTATUS_REBOOTING:
			}
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return "", fmt.Errorf("forge: instance %s not ready after %s: %w", id, p.opts.ReadyTimeout, apiError(err))
			}
			return "", fmt.Errorf("forge: instance %s not ready after %s (last status %q)", id, p.opts.ReadyTimeout, last)
		case <-time.After(p.opts.PollInterval):
		}
	}
}

// waitReachable dials until the node answers. Ready and reachable are
// different signals: an instance reports Ready once Forge has handed it over,
// long before the image is written, booted and running sshd.
func (p *Provider) waitReachable(ctx context.Context, target teleport.Target) (Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ReachTimeout)
	defer cancel()

	var lastErr error
	for {
		conn, err := p.opts.Dialer.Dial(ctx, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("forge: %s not reachable through %s after %s: %w", target.Address, target.JumpHost, p.opts.ReachTimeout, lastErr)
		case <-time.After(p.opts.PollInterval):
		}
	}
}

func (p *Provider) release(ctx context.Context, id string) error {
	ctx, err := p.authorize(ctx)
	if err != nil {
		return err
	}
	resp, err := p.client.InstanceAPI.DeleteInstance(ctx, p.opts.Env.Org.ID, id).Execute()
	closeBody(resp)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting instance %s: %w", id, apiError(err))
	}
	return nil
}

// instanceAddress is the first address on the first interface. The list
// response carries no interfaces at all; only a read by id does.
func instanceAddress(inst *standard.Instance) string {
	for _, iface := range inst.GetInterfaces() {
		for _, address := range iface.GetIpAddresses() {
			if address != "" {
				// Forge reports addresses with their prefix length.
				host, _, _ := strings.Cut(address, "/")
				return host
			}
		}
	}
	return ""
}

// nodeUser is the login for this provider's machines, defaulted once so the
// cloud-init that writes authorized_keys and the dial that uses them cannot
// disagree.
func (p *Provider) nodeUser() string {
	if u := strings.TrimSpace(p.opts.NodeUser); u != "" {
		return u
	}
	return defaultNodeUser
}

func newKey() (ssh.Signer, string, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("forge: generating the session key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, "", fmt.Errorf("forge: generating the session key: %w", err)
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

// closeBody closes the in-memory copy the SDK leaves on a response after
// reading and closing the real body.
func closeBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// apiError surfaces the response body the SDK keeps behind its error type,
// which is where Forge says what it objected to.
func apiError(err error) error {
	var open *standard.GenericOpenAPIError
	if errors.As(err, &open) && len(open.Body()) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(open.Body())))
	}
	return err
}

// session is a Conn plus the release of the machine it is connected to.
type session struct {
	Conn
	release  func(context.Context) error
	released bool
}

func (s *session) Teardown(ctx context.Context) error {
	if s.released {
		return nil
	}
	closeErr := s.Close()
	// Detached for the same reason as a failed provision: teardown is called
	// most often from a run that is already ending.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := s.release(rctx); err != nil {
		return errors.Join(closeErr, err)
	}
	s.released = true
	return closeErr
}
