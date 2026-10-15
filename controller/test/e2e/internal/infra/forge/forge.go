// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package forge provisions the machine a site controller is built on, on
// Forge, through the infra-controller rest-api SDK.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/NVIDIA/infra-controller/rest-api/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/provision"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/teleport"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

const (
	// DefaultEndpoint is production NGC. Staging is
	// https://api.stg.ngc.nvidia.com.
	DefaultEndpoint = "https://api.ngc.nvidia.com"

	// APIName is the path segment Forge serves under, where the SDK's
	// generated paths say carbide.
	APIName = "forge"

	defaultReadyTimeout = 30 * time.Minute
	defaultReachTimeout = 20 * time.Minute
	defaultPollInterval = 30 * time.Second

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

// Options configures a Provider.
type Options struct {
	Env *Environment
	// SiteName is the site to create machines at, by the name it is filed
	// under. Empty resolves to the only site declared.
	SiteName string

	// APIKey is an NGC key. An nvapi- key is the bearer token as-is; a legacy
	// key is exchanged for one.
	APIKey   string
	Endpoint string // empty is DefaultEndpoint
	AuthURL  string // empty pairs the token service with Endpoint

	Images provision.Images
	Dialer provision.Dialer
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
	Node  nvke.NodeInputs
	Tools provision.HostTools

	// KeepOnFailure leaves the machine in place when provisioning fails,
	// rather than releasing it. The node is the only thing that can answer why
	// a boot went wrong -- whether cloud-init ran, what it wrote, what the
	// console saw -- and releasing it destroys that at exactly the moment it
	// is wanted.
	//
	// Off by default, because the alternative to releasing is leaking: a kept
	// machine outlives the run and someone has to delete it by hand. The error
	// says so, and names the address to reach it at.
	KeepOnFailure bool

	// DisableMTLS runs the node agent without SPIFFE. nvkectl bootstrap site
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

// Provider creates Forge instances ready for `nvkectl bootstrap site`.
type Provider struct {
	opts Options
	site Site
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
	_ infra.Session  = (*provision.Session)(nil)
)

// New validates opts against the environment and returns a Provider. It
// makes no API call, so a misconfigured run fails before anything is created.
func New(opts Options) (*Provider, error) {
	if opts.Env == nil {
		return nil, errors.New("forge: no environment")
	}
	site, err := opts.Env.Site(opts.SiteName)
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
	if err := opts.Tools.Validate(); err != nil {
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
	createHTTP := provision.WithTimeout(opts.HTTPClient, opts.CreateTimeout)

	cfg := standard.NewConfiguration()
	cfg.Servers = standard.ServerConfigurations{{URL: endpoint}}
	cfg.HTTPClient = opts.HTTPClient
	// After HTTPClient, because it wraps that client's transport.
	cfg.SetAPIName(APIName)

	// Its own Servers value rather than a shared slice, so neither
	// configuration can be reached through the other's.
	createCfg := standard.NewConfiguration()
	createCfg.Servers = standard.ServerConfigurations{{URL: endpoint}}
	createCfg.HTTPClient = createHTTP
	createCfg.SetAPIName(APIName)

	return &Provider{
		opts:         opts,
		site:         site,
		client:       standard.NewAPIClient(cfg),
		createClient: standard.NewAPIClient(createCfg),
		token:        newTokenSource(opts.HTTPClient, opts.APIKey, opts.Env.Org.ID, opts.AuthURL),
	}, nil
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

	signer, authorizedKey, err := provision.NewKey("forge")
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

	create := standard.NewInstanceCreateRequest(req.Name, tenantID, p.site.VpcID)
	create.SetInterfaces(p.interfaces())
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
	// reached is the node's address once one is known, so a kept machine can
	// be named with somewhere to go rather than only an id.
	var reached string
	fail := func(err error) (infra.Node, infra.Session, error) {
		if p.opts.KeepOnFailure {
			where := "no address was reached"
			if reached != "" {
				where = "at " + reached
			}
			return infra.Node{}, nil, fmt.Errorf("%w\n\tinstance %s (%s) was KEPT for diagnosis, %s -- it outlives this run and must be released by hand", err, req.Name, id, where)
		}
		// Detached, because a canceled run is exactly when this matters most
		// and inheriting the cancellation would abandon the instance.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provision.ReleaseTimeout)
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
	reached = address
	conn, err := provision.WaitReachable(ctx, p.opts.Dialer, teleport.Target{JumpHost: p.opts.JumpHost, Address: address, User: p.nodeUser(), Signer: signer}, "forge", p.opts.ReachTimeout, p.opts.PollInterval)
	if err != nil {
		// A key this run generated moments ago is not rejected by chance.
		// sshd answering and refusing publickey means cloud-init wrote
		// authorized_keys somewhere else, which is what a node.user that
		// does not match the account the OS image bakes looks like. Say so
		// here: the alternative is a bare auth error after the whole
		// provisioning wait, with nothing naming the setting to change.
		if strings.Contains(err.Error(), "unable to authenticate") {
			err = fmt.Errorf("%w\n\t%s answered but refused this run's key, so cloud-init likely wrote authorized_keys for a different account: check node.user (%q) against the account its OS image bakes", err, req.Name, p.nodeUser())
		}
		return fail(err)
	}
	if err := provision.AwaitCloudInit(ctx, conn, "forge", req.Name); err != nil {
		_ = conn.Close()
		return fail(err)
	}

	return infra.Node{Name: req.Name, IP: address, Arch: instanceType.Arch()}, &provision.Session{Conn: conn, Release: release}, nil
}

// payloads returns the iPXE script and the cloud-init user data, in that
// order.
func (p *Provider) payloads(req infra.NodeRequest, instanceType InstanceType, authorizedKey string) (string, string, error) {
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
	node, err := nvke.BuildForgeNodePayloads(inputs)
	if err != nil {
		return "", "", fmt.Errorf("forge: %w", err)
	}
	userData, err := provision.RenderUserData(provision.UserDataInputs{
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

// RequireCapacity fails when a shape this run will create has fewer usable
// allocations than it needs. cpuCount is the site node plus the tenant
// control plane. workerCount is the tenant's workers, taken on the GPU shape
// when the run asked for one and on the CPU shape otherwise.
//
// CAPNICo refuses the create while unusedUsable is zero. Checked here, before
// any instance exists, because the suite would otherwise hold the machines it
// did get until the provision timeout.
func (p *Provider) RequireCapacity(ctx context.Context, capabilities map[string]string, cpuCount, workerCount int) error {
	selection, err := p.opts.Env.Resolve(capabilities)
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}
	type demand struct {
		name string
		id   string
		need int
	}
	needs := map[string]demand{}
	add := func(t InstanceType, n int) {
		if n <= 0 || t.ID == "" {
			return
		}
		d := needs[t.ID]
		d.name = t.Name
		d.id = t.ID
		d.need += n
		needs[t.ID] = d
	}
	add(selection.CPU, cpuCount)
	if workerCount > 0 {
		add(selection.WorkerType(), workerCount)
	}
	if len(needs) == 0 {
		return nil
	}

	ctx, err = p.authorize(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(needs))
	for id := range needs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	problems := make([]string, 0)
	for _, id := range ids {
		d := needs[id]
		item, resp, err := p.client.InstanceTypeAPI.GetInstanceType(ctx, p.opts.Env.Org.ID, d.id).
			IncludeAllocationStats(true).Execute()
		closeBody(resp)
		if err != nil {
			return fmt.Errorf("forge: reading allocation stats for %s (%s): %w", d.name, d.id, apiError(err))
		}
		if !item.HasAllocationStats() {
			return fmt.Errorf("forge: instance type %s (%s) returned no allocation stats", d.name, d.id)
		}
		stats := item.GetAllocationStats()
		usable := int(stats.GetUnusedUsable())
		if usable < d.need {
			problems = append(problems, fmt.Sprintf(
				"%s (%s): need %d, unusedUsable=%d (total=%d used=%d unused=%d)",
				d.name, d.id, d.need, usable, stats.GetTotal(), stats.GetUsed(), stats.GetUnused(),
			))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("forge has no usable allocation for this run:\n%s", strings.Join(problems, "\n"))
	}
	return nil
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
				standard.INSTANCESTATUS_REBOOTING,
				standard.INSTANCESTATUS_REPAIRING:
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

// WaitTerminating returns once Forge reports every one of ids as Terminating
// or no longer has it. That is the point at which Forge owns the release: the
// machine is out of the tenant's hands and comes back to the pool without
// anything else asking, which takes 20 to 60 minutes more on bare metal.
//
// Polled by id for the reason waitReady is. An instance still in any other
// state when ctx ends is reported with the last status Forge gave for it.
func (p *Provider) WaitTerminating(ctx context.Context, ids []string) error {
	pending := slices.Clone(ids)
	slices.Sort(pending)
	pending = slices.Compact(pending)
	last := map[string]string{}
	for {
		actx, err := p.authorize(ctx)
		if err != nil {
			return err
		}
		var next []string
		for _, id := range pending {
			inst, resp, err := p.client.InstanceAPI.GetInstance(actx, p.opts.Env.Org.ID, id).Execute()
			closeBody(resp)
			switch {
			case resp != nil && resp.StatusCode == http.StatusNotFound:
			case err != nil:
				// A read cut off by our own deadline says nothing about the
				// instance, so it does not replace what Forge last said.
				if ctx.Err() == nil || last[id] == "" {
					last[id] = apiError(err).Error()
				}
				next = append(next, id)
			case inst.GetStatus() == standard.INSTANCESTATUS_TERMINATING:
			default:
				last[id] = string(inst.GetStatus())
				next = append(next, id)
			}
		}
		pending = next
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			states := make([]string, 0, len(pending))
			for _, id := range pending {
				states = append(states, fmt.Sprintf("%s (last %s)", id, last[id]))
			}
			return fmt.Errorf("forge: instances not terminating: %s: %w", strings.Join(states, ", "), ctx.Err())
		case <-time.After(p.opts.PollInterval):
		}
	}
}

func (p *Provider) release(ctx context.Context, id string) error {
	ctx, err := p.authorize(ctx)
	if err != nil {
		return err
	}
	_, resp, err := p.client.InstanceAPI.DeleteInstance(ctx, p.opts.Env.Org.ID, id).Execute()
	closeBody(resp)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	// Forge accepts with a plain-text body the SDK cannot decode, so the
	// status is what says whether the delete was taken.
	if resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
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
	return provision.DefaultNodeUser
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

// closeBody closes the in-memory copy the SDK leaves on a response after
// reading and closing the real body.
//
// Per package rather than shared: bodyclose is an SSA analyzer and cannot
// follow the close through a call into another package, so a shared helper
// turns every call site into a false positive.
func closeBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}
