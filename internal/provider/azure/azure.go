// Package azure is a Provider on Azure virtual machines: per machine one network interface with a private address
// only, and one VM from a Compute Gallery image whose OS disk and network interface are deleted with it.
package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v8"

	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/provider"
)

// Tags on every VM and network interface matchstick creates.
const (
	TagInstance  = "matchstick-instance"
	TagPool      = "matchstick-pool"
	TagCreatedAt = "matchstick-created-at"
	TagExpiresAt = "matchstick-expires-at"
	TagPriority  = "matchstick-priority"
)

const (
	// allocationWait is how long Create waits for Azure to allocate a VM, so that capacity failures are reported
	// to the pool; a VM still being created after it is left to finish.
	allocationWait = time.Minute
	// spotRest is how long a pool uses regular priority after Spot had no capacity.
	spotRest = 30 * time.Minute
	// leftoverAge is how old a network interface without its VM must be to count as failed rather than pending.
	leftoverAge = 2 * time.Minute
	adminUser   = "runner"
)

// Cloud is where the machines go, shared by every pool.
type Cloud struct {
	SubscriptionID string
	ResourceGroup  string
	Location       string
	SubnetID       string
}

// Shape is one pool's machine shape.
type Shape struct {
	Size                  string
	Image                 string
	Identity              string
	EphemeralOSDisk       string
	OSDiskSizeGB          int32
	Spot                  bool
	AcceleratedNetworking bool
	CustomData            []byte
}

// Provider manages one pool's machines.
type Provider struct {
	cloud    Cloud
	shape    Shape
	instance string
	pool     string
	vms      *armcompute.VirtualMachinesClient
	nics     *armnetwork.InterfacesClient
	attest   *attestation
	now      func() time.Time

	mu        sync.Mutex
	spotAfter time.Time
}

var _ provider.Provider = (*Provider)(nil)

// New returns the provider of one pool.
func New(cloud Cloud, shape Shape, instance, pool string, cred azcore.TokenCredential,
	opts *arm.ClientOptions) (*Provider, error) {
	vms, err := armcompute.NewVirtualMachinesClient(cloud.SubscriptionID, cred, opts)
	if err != nil {
		return nil, err
	}
	nics, err := armnetwork.NewInterfacesClient(cloud.SubscriptionID, cred, opts)
	if err != nil {
		return nil, err
	}
	roots, err := azureRoots()
	if err != nil {
		return nil, err
	}
	return &Provider{cloud: cloud, shape: shape, instance: instance, pool: pool, vms: vms, nics: nics,
		attest: newAttestation(roots, httpDownload()), now: time.Now}, nil
}

func nicName(machine string) string { return machine + "-nic" }

// Create creates the network interface, then the VM, and waits up to a minute for the VM's allocation.
func (p *Provider) Create(ctx context.Context, spec provider.CreateSpec) error {
	now := p.now().UTC()
	spot := p.shape.Spot && !now.Before(p.spotUntil())
	tags := p.tags(now, spec.ExpiresAt, spot)

	nicPoller, err := p.nics.BeginCreateOrUpdate(ctx, p.cloud.ResourceGroup, nicName(spec.Name),
		p.nic(tags), nil)
	if err != nil {
		return classify(err, false)
	}
	nic, err := nicPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return classify(err, false)
	}

	vm, err := p.vm(spec, *nic.ID, tags, spot)
	if err != nil {
		return err
	}
	vmPoller, err := p.vms.BeginCreateOrUpdate(ctx, p.cloud.ResourceGroup, spec.Name, vm, nil)
	if err != nil {
		// Azure did not accept the VM, so its network interface is all there is.
		if _, delErr := p.nics.BeginDelete(context.WithoutCancel(ctx), p.cloud.ResourceGroup, nicName(spec.Name),
			nil); delErr != nil && !notFound(delErr) {
			err = errors.Join(err, delErr)
		}
		return p.failed(err, spot)
	}
	wait, cancel := context.WithTimeout(ctx, allocationWait)
	defer cancel()
	if _, err := vmPoller.PollUntilDone(wait, nil); err != nil && wait.Err() == nil {
		return p.failed(err, spot)
	}
	return nil
}

// failed reports a VM creation failure. A Spot VM without capacity makes the pool use regular priority for a while;
// the pool's next round creates the replacement.
func (p *Provider) failed(err error, spot bool) error {
	if spot && capacity(err) {
		p.mu.Lock()
		p.spotAfter = p.now().Add(spotRest)
		p.mu.Unlock()
		return fmt.Errorf("azure: no Spot capacity, using regular priority for %s: %w", spotRest, err)
	}
	return classify(err, true)
}

func (p *Provider) spotUntil() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spotAfter
}

func (p *Provider) tags(now, expires time.Time, spot bool) map[string]*string {
	priority := "regular"
	if spot {
		priority = "spot"
	}
	return map[string]*string{
		TagInstance:  new(p.instance),
		TagPool:      new(p.pool),
		TagCreatedAt: new(now.Format(time.RFC3339)),
		TagExpiresAt: new(expires.UTC().Format(time.RFC3339)),
		TagPriority:  new(priority),
	}
}

func (p *Provider) nic(tags map[string]*string) armnetwork.Interface {
	return armnetwork.Interface{
		Location: new(p.cloud.Location),
		Tags:     tags,
		Properties: &armnetwork.InterfacePropertiesFormat{
			EnableAcceleratedNetworking: new(p.shape.AcceleratedNetworking),
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: new("primary"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					Primary:                   new(true),
					PrivateIPAllocationMethod: new(armnetwork.IPAllocationMethodDynamic),
					Subnet:                    &armnetwork.Subnet{ID: new(p.cloud.SubnetID)},
				},
			}},
		},
	}
}

func (p *Provider) vm(spec provider.CreateSpec, nicID string, tags map[string]*string,
	spot bool) (armcompute.VirtualMachine, error) {
	key, err := unusableSSHKey()
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}
	profile := &armcompute.OSProfile{
		ComputerName:  new(spec.Name),
		AdminUsername: new(adminUser),
		LinuxConfiguration: &armcompute.LinuxConfiguration{
			DisablePasswordAuthentication: new(true),
			SSH: &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{
				Path: new("/home/" + adminUser + "/.ssh/authorized_keys"), KeyData: new(key),
			}}},
		},
	}
	if len(p.shape.CustomData) > 0 {
		profile.CustomData = new(base64.StdEncoding.EncodeToString(p.shape.CustomData))
	}
	vm := armcompute.VirtualMachine{
		Location: new(p.cloud.Location),
		Tags:     tags,
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: new(armcompute.VirtualMachineSizeTypes(p.shape.Size))},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{ID: new(p.shape.Image)},
				OSDisk:         p.osDisk(spec.Name),
			},
			OSProfile: profile,
			UserData:  userData(spec.Bootstrap),
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
					ID: new(nicID),
					Properties: &armcompute.NetworkInterfaceReferenceProperties{
						Primary: new(true), DeleteOption: new(armcompute.DeleteOptionsDelete),
					},
				}},
			},
			DiagnosticsProfile: &armcompute.DiagnosticsProfile{
				BootDiagnostics: &armcompute.BootDiagnostics{Enabled: new(true)},
			},
		},
	}
	if p.shape.Identity != "" {
		vm.Identity = &armcompute.VirtualMachineIdentity{
			Type:                   new(armcompute.ResourceIdentityTypeUserAssigned),
			UserAssignedIdentities: map[string]*armcompute.UserAssignedIdentitiesValue{p.shape.Identity: {}},
		}
	}
	if spot {
		vm.Properties.Priority = new(armcompute.VirtualMachinePriorityTypesSpot)
		vm.Properties.EvictionPolicy = new(armcompute.VirtualMachineEvictionPolicyTypesDelete)
		vm.Properties.BillingProfile = &armcompute.BillingProfile{MaxPrice: new(-1.0)}
	}
	return vm, nil
}

func (p *Provider) osDisk(machine string) *armcompute.OSDisk {
	disk := &armcompute.OSDisk{
		Name:         new(machine + "-os"),
		CreateOption: new(armcompute.DiskCreateOptionTypesFromImage),
		DeleteOption: new(armcompute.DiskDeleteOptionTypesDelete),
		Caching:      new(armcompute.CachingTypesReadWrite),
	}
	if p.shape.OSDiskSizeGB > 0 {
		disk.DiskSizeGB = new(p.shape.OSDiskSizeGB)
	}
	if p.shape.EphemeralOSDisk != "" {
		disk.Caching = new(armcompute.CachingTypesReadOnly)
		disk.DiffDiskSettings = &armcompute.DiffDiskSettings{
			Option:    new(armcompute.DiffDiskOptionsLocal),
			Placement: new(armcompute.DiffDiskPlacement(p.shape.EphemeralOSDisk)),
		}
	}
	return disk
}

// List returns the pool's VMs, and its network interfaces left without a VM.
func (p *Provider) List(ctx context.Context) ([]plan.Machine, error) {
	machines := []plan.Machine{}
	seen := map[string]bool{}
	vms := p.vms.NewListPager(p.cloud.ResourceGroup, nil)
	for vms.More() {
		page, err := vms.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: listing VMs: %w", err)
		}
		for _, vm := range page.Value {
			if vm == nil || vm.Name == nil || !p.owns(vm.Tags) {
				continue
			}
			var view *armcompute.VirtualMachineInstanceView
			if vm.Properties != nil && deref(vm.Properties.ProvisioningState) == "Succeeded" {
				v, err := p.vms.InstanceView(ctx, p.cloud.ResourceGroup, *vm.Name, nil)
				if err != nil && !notFound(err) {
					return nil, fmt.Errorf("azure: VM %s: %w", *vm.Name, err)
				}
				view = &v.VirtualMachineInstanceView
			}
			seen[*vm.Name] = true
			machines = append(machines, machine(*vm, view))
		}
	}
	nics := p.nics.NewListPager(p.cloud.ResourceGroup, nil)
	for nics.More() {
		page, err := nics.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: listing network interfaces: %w", err)
		}
		for _, nic := range page.Value {
			if nic == nil || nic.Name == nil || !p.owns(nic.Tags) {
				continue
			}
			name, ok := strings.CutSuffix(*nic.Name, "-nic")
			if !ok || seen[name] {
				continue
			}
			machines = append(machines, leftover(name, nic, p.now()))
		}
	}
	return machines, nil
}

func (p *Provider) owns(tags map[string]*string) bool {
	return deref(tags[TagInstance]) == p.instance && deref(tags[TagPool]) == p.pool
}

// Delete starts deleting the VM, which takes its OS disk and network interface with it, or the network interface if no
// VM is left. It returns once Azure has accepted the deletion.
func (p *Provider) Delete(ctx context.Context, name string) error {
	vm, err := p.vms.Get(ctx, p.cloud.ResourceGroup, name, nil)
	switch {
	case err == nil:
		if !p.owns(vm.Tags) {
			return fmt.Errorf("azure: VM %s is not this pool's", name)
		}
		if _, err := p.vms.BeginDelete(ctx, p.cloud.ResourceGroup, name, nil); err != nil && !notFound(err) {
			return fmt.Errorf("azure: deleting VM %s: %w", name, err)
		}
		return nil
	case !notFound(err):
		return fmt.Errorf("azure: VM %s: %w", name, err)
	}
	nic, err := p.nics.Get(ctx, p.cloud.ResourceGroup, nicName(name), nil)
	switch {
	case notFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("azure: network interface of %s: %w", name, err)
	case !p.owns(nic.Tags):
		return fmt.Errorf("azure: network interface of %s is not this pool's", name)
	}
	if _, err := p.nics.BeginDelete(ctx, p.cloud.ResourceGroup, nicName(name), nil); err != nil && !notFound(err) {
		return fmt.Errorf("azure: deleting network interface of %s: %w", name, err)
	}
	return nil
}

// Identify checks that proof is an attested document of the VM name, which must be one of the pool's. See
// attestation.check for the document's checks.
func (p *Provider) Identify(ctx context.Context, name string, proof json.RawMessage) error {
	doc, err := p.attest.check(ctx, proof, p.cloud.SubscriptionID, p.now())
	if err != nil {
		return err
	}
	vm, err := p.vms.Get(ctx, p.cloud.ResourceGroup, name, nil)
	switch {
	case notFound(err):
		return notProven("no VM %s", name)
	case err != nil:
		return fmt.Errorf("azure: VM %s: %w", name, err)
	case !p.owns(vm.Tags):
		return notProven("VM %s is not this pool's", name)
	case vm.Properties == nil || !strings.EqualFold(deref(vm.Properties.VMID), doc.VMID):
		return notProven("the document's vmId %s is not VM %s's", doc.VMID, name)
	}
	return nil
}

// userData encodes a machine's bootstrap as user data; none if it is empty.
func userData(bootstrap []byte) *string {
	if len(bootstrap) == 0 {
		return nil
	}
	return new(base64.StdEncoding.EncodeToString(bootstrap))
}

// machine maps a VM and its instance view to the core's machine.
func machine(vm armcompute.VirtualMachine, view *armcompute.VirtualMachineInstanceView) plan.Machine {
	m := plan.Machine{Name: deref(vm.Name), State: plan.Pending, ExpiresAt: tagTime(vm.Tags, TagExpiresAt),
		CreatedAt: tagTime(vm.Tags, TagCreatedAt)}
	if vm.Properties == nil {
		return m
	}
	if vm.Properties.TimeCreated != nil {
		m.CreatedAt = vm.Properties.TimeCreated.UTC()
	}
	switch deref(vm.Properties.ProvisioningState) {
	case "Deleting":
		m.State = plan.Deleting
		return m
	case "Failed":
		m.State = plan.Failed
		return m
	}
	if view == nil {
		return m
	}
	for _, s := range view.Statuses {
		switch deref(s.Code) {
		case "PowerState/running":
			m.State = plan.Running
		case "PowerState/stopping", "PowerState/stopped", "PowerState/deallocating", "PowerState/deallocated":
			m.State = plan.Stopped
		}
	}
	return m
}

// leftover maps a network interface without its VM: pending while young (its VM may be about to appear), failed after.
func leftover(name string, nic *armnetwork.Interface, now time.Time) plan.Machine {
	m := plan.Machine{Name: name, State: plan.Failed, CreatedAt: tagTime(nic.Tags, TagCreatedAt),
		ExpiresAt: tagTime(nic.Tags, TagExpiresAt)}
	if nic.Properties != nil && deref(nic.Properties.ProvisioningState) == armnetwork.ProvisioningStateDeleting {
		m.State = plan.Deleting
	} else if now.Sub(m.CreatedAt) < leftoverAge {
		m.State = plan.Pending
	}
	return m
}

func tagTime(tags map[string]*string, key string) time.Time {
	t, err := time.Parse(time.RFC3339, deref(tags[key]))
	if err != nil {
		return time.Time{}
	}
	return t
}

// capacityCodes are Azure error codes for a refusal for lack of quota or capacity.
var capacityCodes = map[string]bool{
	"QuotaExceeded":                         true,
	"SkuNotAvailable":                       true,
	"AllocationFailed":                      true,
	"ZonalAllocationFailed":                 true,
	"OverconstrainedAllocationRequest":      true,
	"OverconstrainedZonalAllocationRequest": true,
}

// capacity reports whether err is Azure refusing for lack of quota or capacity.
func capacity(err error) bool {
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		return false
	}
	if capacityCodes[re.ErrorCode] {
		return true
	}
	// Core quota refusals come as OperationNotAllowed with the quota named in the message.
	return re.ErrorCode == "OperationNotAllowed" && strings.Contains(strings.ToLower(re.Error()), "quota")
}

// classify wraps capacity refusals in provider.ErrCapacity.
func classify(err error, vm bool) error {
	what := "network interface"
	if vm {
		what = "VM"
	}
	if capacity(err) {
		return fmt.Errorf("azure: creating %s: %w: %w", what, provider.ErrCapacity, err)
	}
	return fmt.Errorf("azure: creating %s: %w", what, err)
}

func notFound(err error) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
