// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
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
	// APIName is the path segment a DSX site serves under, where the SDK's
	// generated paths say carbide. Forge serves the same API under "forge",
	// which is the whole of the difference at this layer.
	APIName = "nico"

	// The two waits are weighted the other way round from Forge's, because on
	// this substrate they wait for different things than they do there.
	//
	// An address arrives early: BootCompleted is reported about three and a
	// half minutes after create, before the host has finished imaging, and the
	// interface has its address by then. waitAddressed wants nothing more, so
	// fifteen minutes is already generous.
	//
	// SSH is the long one. After the imager writes the disk and reboots, the
	// firmware loops UEFI HTTP boot across every NIC before falling through to
	// disk -- ten, fifteen and twenty-seven minutes were all measured during
	// an earlier bring-up. Slow rather than broken, so WaitReachable carries
	// the budget.
	//
	// Together they are 50 minutes, matching Forge's 30 + 20, and both sit
	// under the same governor: the provision spec's own NodeTimeout, which is
	// 45 minutes for every provider. These bound the individual waits; that
	// bounds the pair. A run that hits it still releases its machine, because
	// the release on failure is detached from the canceled context.
	defaultReadyTimeout = 15 * time.Minute
	defaultReachTimeout = 35 * time.Minute
	defaultPollInterval = 30 * time.Second

	// defaultCreateTimeout bounds the create call alone, and is deliberately
	// far longer than the timeout the polls run under. Losing the response to
	// a create is the one unrecoverable failure here: the id it carries is the
	// only handle on the instance, and the machine is built whether or not the
	// answer arrives.
	defaultCreateTimeout = 10 * time.Minute

	// createMayHaveLanded is appended to every create failure, for the same
	// reason it is on the Forge side: nothing in this process can clean up
	// after one.
	createMayHaveLanded = "\nthe instance may exist even so: the site builds it before it answers, and without the id in that answer nothing here can delete it -- check the datahall for this name"
)

// Options configures a Provider.
type Options struct {
	Env *Environment
	// SiteName is the datahall to create machines at, by the name it is filed
	// under. Empty resolves to the only one declared.
	SiteName string

	// Issuer is the SSA issuer base url, which the exchange appends "/token"
	// to --- not the token endpoint itself. Empty takes SSA_ISSUER from the
	// environment, and a run with neither fails before anything is created.
	Issuer string

	Images provision.Images
	Dialer provision.Dialer

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

	// DisableMTLS runs the node agent without SPIFFE. `nvkectl bootstrap site`
	// must be given --disable-mtls to match, and the stack deployed with
	// spiffe.enabled=false, or every dial fails as a TLS handshake error.
	DisableMTLS bool

	HTTPClient   *http.Client
	ReadyTimeout time.Duration
	ReachTimeout time.Duration
	PollInterval time.Duration
	// CreateTimeout bounds the create call, separately from HTTPClient's
	// timeout, which bounds everything else.
	CreateTimeout time.Duration
}

// Provider creates instances on a DSX datahall, ready for
// `nvkectl bootstrap site`.
type Provider struct {
	opts Options
	site Site
	// client serves everything but create. Its short per-request timeout is
	// what makes a hung poll fail and retry rather than stall.
	client *standard.APIClient
	// createClient serves create alone, under a far longer timeout, because
	// that call's response cannot be given up on.
	createClient *standard.APIClient
	token        *tokenSource
}

var _ infra.Provider = (*Provider)(nil)

// New validates opts against the environment and returns a Provider. It makes
// no API call, so a misconfigured run fails before anything is created.
func New(opts Options) (*Provider, error) {
	if opts.Env == nil {
		return nil, errors.New("dsx: no environment")
	}
	site, err := opts.Env.Site(opts.SiteName)
	if err != nil {
		return nil, fmt.Errorf("dsx: %w", err)
	}
	if strings.TrimSpace(site.JumpHost) == "" {
		return nil, fmt.Errorf("dsx: no jump host for datahall %q, and its machines are reachable only through one", site.Key)
	}
	if opts.Images == nil {
		return nil, errors.New("dsx: no image resolver")
	}
	if opts.Dialer == nil {
		return nil, errors.New("dsx: no dialer")
	}
	if err := opts.Tools.Validate(); err != nil {
		return nil, fmt.Errorf("dsx: %w", err)
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
	if strings.TrimSpace(opts.Issuer) == "" {
		opts.Issuer = os.Getenv(EnvIssuer)
	}

	// The datahall addresses itself: the key it is filed under is in the host.
	endpoint := site.Endpoint()
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
		token:        newTokenSource(opts.HTTPClient, opts.Issuer, site.Key),
	}, nil
}

// ProvisionNode creates one instance and returns once a session to it can run
// commands and cloud-init has finished. An instance created and then not
// brought that far is deleted before the error is returned, so a failed run
// does not leak hardware.
func (p *Provider) ProvisionNode(ctx context.Context, req infra.NodeRequest) (infra.Node, infra.Session, error) {
	if strings.TrimSpace(req.Name) == "" {
		return infra.Node{}, nil, errors.New("dsx: node request has no name")
	}
	// Where the node goes: an instance type, so the datahall picks a machine
	// that is free and healthy, unless the site pins one. See Site.Place.
	arch := req.Capabilities[infra.CapabilityArch]
	if strings.TrimSpace(arch) == "" {
		arch = infra.DefaultArch
	}
	place, err := p.site.Place(arch)
	if err != nil {
		return infra.Node{}, nil, fmt.Errorf("dsx: %w", err)
	}

	signer, authorizedKey, err := provision.NewKey("dsx")
	if err != nil {
		return infra.Node{}, nil, err
	}
	ipxe, userData, err := p.payloads(req, place, authorizedKey)
	if err != nil {
		return infra.Node{}, nil, err
	}

	ctx, err = p.authorize(ctx)
	if err != nil {
		return infra.Node{}, nil, err
	}

	// The tenant is declared rather than read back. Forge resolves it from
	// GET /tenant/current; here it belongs to the datahall and is in the file
	// with the rest of its identifiers.
	create := standard.NewInstanceCreateRequest(req.Name, p.site.TenantID, p.site.VpcID)
	create.SetInterfaces(p.interfaces())
	// One or the other, never both: machineId names a host and needs the
	// tenant's targetedInstanceCreation capability, instanceTypeId leaves the
	// choice to the site.
	if place.MachineID != "" {
		create.SetMachineId(place.MachineID)
	} else {
		create.SetInstanceTypeId(place.InstanceTypeID)
	}
	create.SetIpxeScript(ipxe)
	create.SetUserData(userData)
	// False, where Forge sets true. True makes the firmware re-run the custom
	// iPXE on every boot instead of booting the disk it just wrote, so the
	// node images itself forever.
	create.SetAlwaysBootWithCustomIpxe(false)
	// A workaround, not a choice, and the same one Forge needs: phone-home is
	// what flips an instance from Provisioning to Ready, and the metadata
	// endpoint has been unreachable from new instances since 2026-09-19, so
	// the callback never completes. With it off, Ready means less than it
	// already did here, which is why the node is held until cloud-init reports
	// done over the session.
	create.SetPhoneHomeEnabled(false)
	create.SetSshKeyGroupIds([]string{p.site.SSHKeyGroupID})
	if len(req.Labels) > 0 {
		create.SetLabels(req.Labels)
	}

	inst, resp, err := p.createClient.InstanceAPI.CreateInstance(ctx, p.opts.Env.Org.ID).InstanceCreateRequest(*create).Execute()
	closeBody(resp)
	if err != nil {
		return infra.Node{}, nil, fmt.Errorf("dsx: creating instance %s on %s: %w%s", req.Name, place.Name, apiError(err), createMayHaveLanded)
	}
	if inst.GetId() == "" {
		return infra.Node{}, nil, fmt.Errorf("dsx: creating instance %s returned no id%s", req.Name, createMayHaveLanded)
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
			return infra.Node{}, nil, errors.Join(err, fmt.Errorf("dsx: instance %s (%s) was not deleted and must be removed by hand: %w", req.Name, id, rerr))
		}
		return infra.Node{}, nil, err
	}

	address, err := p.waitAddressed(ctx, id)
	if err != nil {
		return fail(err)
	}
	reached = address
	target := teleport.Target{JumpHost: p.site.JumpHost, Address: address, User: p.nodeUser(), Signer: signer}
	conn, err := provision.WaitReachable(ctx, p.opts.Dialer, target, "dsx", p.opts.ReachTimeout, p.opts.PollInterval)
	if err != nil {
		// A key this run generated moments ago is not rejected by chance.
		// sshd answering and refusing publickey means cloud-init wrote
		// authorized_keys somewhere else, which is what a nodeUser that does
		// not match the account the OS image bakes looks like.
		if strings.Contains(err.Error(), "unable to authenticate") {
			err = fmt.Errorf("%w\n\t%s answered but refused this run's key, so cloud-init likely wrote authorized_keys for a different account: check the datahall's nodeUser (%q) against the account its OS image bakes", err, req.Name, p.nodeUser())
		}
		return fail(err)
	}
	if err := provision.AwaitCloudInit(ctx, conn, "dsx", req.Name); err != nil {
		_ = conn.Close()
		return fail(err)
	}

	return infra.Node{Name: req.Name, IP: address, Arch: place.Arch}, &provision.Session{Conn: conn, Release: release}, nil
}

// payloads returns the iPXE script and the cloud-init user data, in that order.
func (p *Provider) payloads(req infra.NodeRequest, place Placement, authorizedKey string) (string, string, error) {
	arch := place.Arch
	if arch == "" {
		return "", "", fmt.Errorf("dsx: %s declares no arch, so neither boot parameters nor host tools can be chosen for it", place.Name)
	}
	bootLoader, image, err := p.opts.Images(req.NodeVersion, arch)
	if err != nil {
		return "", "", fmt.Errorf("dsx: resolving the %s image for node version %q: %w", arch, req.NodeVersion, err)
	}

	inputs := p.opts.Node
	inputs.BootLoaderURL, inputs.ImageURL = bootLoader, image
	inputs.Arch = arch
	if p.opts.DisableMTLS {
		inputs.AgentArgs = "--secure-server-port 15005 --disable-mtls"
	}
	// The same payload builder the Forge lane uses. Nothing in it is Forge's:
	// it adds the apt source, installs the kubeadm distro and BOTH OVN
	// packages, and renders the iPXE the instance boots. A datahall changes
	// the inputs, not the recipe.
	node, err := nvke.BuildForgeNodePayloads(inputs)
	if err != nil {
		return "", "", fmt.Errorf("dsx: %w", err)
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
		return "", "", fmt.Errorf("dsx: %w", err)
	}
	return node.IPXEScript, userData, nil
}

// interfaces attaches the instance to the datahall's network. Always a prefix:
// a DSX fabric is FNN, which exposes prefixes rather than subnets, so there is
// nothing to choose between the way Forge has to.
func (p *Provider) interfaces() []standard.InterfaceCreateRequest {
	iface := standard.NewInterfaceCreateRequest()
	iface.SetVpcPrefixId(p.site.VpcPrefixID)
	iface.SetIsPhysical(true)
	return []standard.InterfaceCreateRequest{*iface}
}

// RequireCapacity fails when a shape this run will create has fewer usable
// allocations than it needs. cpuCount is the site node plus the tenant control
// plane. workerCount is the tenant's workers, taken on the GPU shape when the
// run asked for one and on the CPU shape otherwise.
//
// The datahall gates on unusedUsable the way Forge does, and reports it from
// the same field, so this asks the same question of the same API. What differs
// is that a datahall with no CPU machine resolves both roles to one instance
// type -- Resolve fills CPU from matchSiteControllerType, which falls back to a
// GPU shape -- so the two demands land on the same id and have to sum rather
// than be checked apart. The map is keyed by id for that reason.
//
// Checked before any instance exists, because the suite would otherwise hold
// the machines it did get until the provision timeout, and on a datahall whose
// every machine is a tray that is capacity nothing else can use meanwhile.
func (p *Provider) RequireCapacity(ctx context.Context, capabilities map[string]string, cpuCount, workerCount int) error {
	selection, err := p.opts.Env.Resolve(capabilities)
	if err != nil {
		return fmt.Errorf("dsx: %w", err)
	}
	type demand struct {
		name string
		id   string
		need int
	}
	needs := map[string]demand{}
	add := func(instanceType InstanceType, n int) {
		if n <= 0 || instanceType.ID == "" {
			return
		}
		d := needs[instanceType.ID]
		d.name = instanceType.Name
		d.id = instanceType.ID
		d.need += n
		needs[instanceType.ID] = d
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
			return fmt.Errorf("dsx: reading allocation stats for %s (%s): %w", d.name, d.id, apiError(err))
		}
		if !item.HasAllocationStats() {
			return fmt.Errorf("dsx: instance type %s (%s) returned no allocation stats", d.name, d.id)
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
		return fmt.Errorf("the datahall has no usable allocation for this run:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

func (p *Provider) authorize(ctx context.Context) (context.Context, error) {
	token, err := p.token.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("dsx: %w", err)
	}
	return context.WithValue(ctx, standard.ContextAccessToken, token), nil
}

// waitAddressed polls until the instance has an address to dial.
//
// It does NOT wait for Ready, because here that means very little: it is
// reported about three and a half minutes after create, before the host has
// finished imaging. SSH answering is the real signal and WaitReachable is what
// waits for it, so this only needs to get far enough to know where to dial.
// Error and Terminating still end the wait --- those say the machine is not
// coming.
func (p *Provider) waitAddressed(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ReadyTimeout)
	defer cancel()

	var last standard.InstanceStatus
	for {
		// Authorized per poll: this loop outlives a token, which on this
		// substrate lives fifteen minutes.
		actx, err := p.authorize(ctx)
		if err != nil {
			return "", err
		}
		inst, resp, err := p.client.InstanceAPI.GetInstance(actx, p.opts.Env.Org.ID, id).Execute()
		closeBody(resp)
		if err == nil {
			last = inst.GetStatus()
			switch last {
			case standard.INSTANCESTATUS_ERROR, standard.INSTANCESTATUS_TERMINATING:
				return "", fmt.Errorf("dsx: instance %s went to %s while provisioning", id, last)
			// Repairing is the datahall working on the machine. Not fatal, so
			// the poll continues, but no address is taken from it either: an
			// instance under repair is not one to hand a run. If it never
			// leaves, the timeout below names Repairing, which says what to
			// look at.
			case standard.INSTANCESTATUS_REPAIRING:
			// Every other status can carry an address, including Ready --
			// which here means only that the datahall handed the machine over.
			case standard.INSTANCESTATUS_PENDING,
				standard.INSTANCESTATUS_PROVISIONING,
				standard.INSTANCESTATUS_CONFIGURING,
				standard.INSTANCESTATUS_READY,
				standard.INSTANCESTATUS_UPDATING,
				standard.INSTANCESTATUS_REBOOTING:
				if address := instanceAddress(inst); address != "" {
					return address, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return "", fmt.Errorf("dsx: instance %s had no address after %s: %w", id, p.opts.ReadyTimeout, apiError(err))
			}
			return "", fmt.Errorf("dsx: instance %s had no address after %s (last status %q)", id, p.opts.ReadyTimeout, last)
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
	// The API accepts with a plain-text body the SDK cannot decode, so the
	// status is what says whether the delete was taken.
	if resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if err != nil {
		// A delete here can take 45 minutes and holds the name while it does,
		// so a caller that needs the name again should take a new one rather
		// than wait.
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
				// Addresses are reported with their prefix length.
				host, _, _ := strings.Cut(address, "/")
				return host
			}
		}
	}
	return ""
}

// nodeUser is the login for this datahall's machines, defaulted once so the
// cloud-init that writes authorized_keys and the dial that uses them cannot
// disagree.
func (p *Provider) nodeUser() string {
	if u := strings.TrimSpace(p.site.NodeUser); u != "" {
		return u
	}
	return provision.DefaultNodeUser
}

// apiError surfaces the response body the SDK keeps behind its error type,
// which is where the site says what it objected to.
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
