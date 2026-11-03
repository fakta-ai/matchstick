package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	computefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v8"
	networkfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v8/fake"

	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/provider"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

var cloud = Cloud{SubscriptionID: "00000000-0000-0000-0000-000000000000", ResourceGroup: "runners",
	Location: "westeurope", SubnetID: "/subscriptions/s/resourceGroups/n/providers/Microsoft.Network/virtualNetworks/v/subnets/runners"}

// azureWorld is a fake Azure resource group: what was created, deleted, and what the next calls return.
type azureWorld struct {
	mu          sync.Mutex
	vms         map[string]armcompute.VirtualMachine
	views       map[string]string // power state code per VM
	nics        map[string]armnetwork.Interface
	created     []armcompute.VirtualMachine
	deletedVMs  []string
	deletedNICs []string
	createErr   []string // error codes for the next VM PUTs, refused before acceptance
	allocErr    []string // error codes for the next VM creations, failing after acceptance
}

func newProvider(t *testing.T, shape Shape) (*Provider, *azureWorld) {
	t.Helper()
	w := &azureWorld{vms: map[string]armcompute.VirtualMachine{}, views: map[string]string{},
		nics: map[string]armnetwork.Interface{}}
	vmServer := computefake.VirtualMachinesServer{
		BeginCreateOrUpdate: func(_ context.Context, _, name string, vm armcompute.VirtualMachine,
			_ *armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions) (
			resp azfake.PollerResponder[armcompute.VirtualMachinesClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if len(w.createErr) > 0 {
				errResp.SetResponseError(http.StatusConflict, w.createErr[0])
				w.createErr = w.createErr[1:]
				return
			}
			vm.Name = new(name)
			w.created = append(w.created, vm)
			if len(w.allocErr) > 0 {
				resp.SetTerminalError(http.StatusConflict, w.allocErr[0])
				w.allocErr = w.allocErr[1:]
				vm.Properties.ProvisioningState = new("Failed")
				w.vms[name] = vm
				return
			}
			vm.Properties.ProvisioningState = new("Succeeded")
			w.vms[name] = vm
			resp.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachinesClientCreateOrUpdateResponse{VirtualMachine: vm}, nil)
			return
		},
		NewListPager: func(string, *armcompute.VirtualMachinesClientListOptions) (
			resp azfake.PagerResponder[armcompute.VirtualMachinesClientListResponse]) {
			w.mu.Lock()
			defer w.mu.Unlock()
			var page []*armcompute.VirtualMachine
			for _, vm := range w.vms {
				page = append(page, new(vm))
			}
			resp.AddPage(http.StatusOK, armcompute.VirtualMachinesClientListResponse{
				VirtualMachineListResult: armcompute.VirtualMachineListResult{Value: page}}, nil)
			return
		},
		InstanceView: func(_ context.Context, _, name string, _ *armcompute.VirtualMachinesClientInstanceViewOptions) (
			resp azfake.Responder[armcompute.VirtualMachinesClientInstanceViewResponse], errResp azfake.ErrorResponder) {
			w.mu.Lock()
			defer w.mu.Unlock()
			resp.SetResponse(http.StatusOK, armcompute.VirtualMachinesClientInstanceViewResponse{
				VirtualMachineInstanceView: armcompute.VirtualMachineInstanceView{Statuses: []*armcompute.InstanceViewStatus{
					{Code: new("ProvisioningState/succeeded")}, {Code: new(w.views[name])}}}}, nil)
			return
		},
		Get: func(_ context.Context, _, name string, _ *armcompute.VirtualMachinesClientGetOptions) (
			resp azfake.Responder[armcompute.VirtualMachinesClientGetResponse], errResp azfake.ErrorResponder) {
			w.mu.Lock()
			defer w.mu.Unlock()
			vm, ok := w.vms[name]
			if !ok {
				errResp.SetResponseError(http.StatusNotFound, "ResourceNotFound")
				return
			}
			resp.SetResponse(http.StatusOK, armcompute.VirtualMachinesClientGetResponse{VirtualMachine: vm}, nil)
			return
		},
		BeginDelete: func(_ context.Context, _, name string, _ *armcompute.VirtualMachinesClientBeginDeleteOptions) (
			resp azfake.PollerResponder[armcompute.VirtualMachinesClientDeleteResponse], errResp azfake.ErrorResponder) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.deletedVMs = append(w.deletedVMs, name)
			delete(w.vms, name)
			resp.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachinesClientDeleteResponse{}, nil)
			return
		},
	}
	nicServer := networkfake.InterfacesServer{
		BeginCreateOrUpdate: func(_ context.Context, _, name string, nic armnetwork.Interface,
			_ *armnetwork.InterfacesClientBeginCreateOrUpdateOptions) (
			resp azfake.PollerResponder[armnetwork.InterfacesClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			w.mu.Lock()
			defer w.mu.Unlock()
			nic.Name = new(name)
			nic.ID = new("/subscriptions/s/resourceGroups/runners/providers/Microsoft.Network/networkInterfaces/" + name)
			w.nics[name] = nic
			resp.SetTerminalResponse(http.StatusOK, armnetwork.InterfacesClientCreateOrUpdateResponse{Interface: nic}, nil)
			return
		},
		NewListPager: func(string, *armnetwork.InterfacesClientListOptions) (
			resp azfake.PagerResponder[armnetwork.InterfacesClientListResponse]) {
			w.mu.Lock()
			defer w.mu.Unlock()
			var page []*armnetwork.Interface
			for _, nic := range w.nics {
				page = append(page, new(nic))
			}
			resp.AddPage(http.StatusOK, armnetwork.InterfacesClientListResponse{
				InterfaceListResult: armnetwork.InterfaceListResult{Value: page}}, nil)
			return
		},
		Get: func(_ context.Context, _, name string, _ *armnetwork.InterfacesClientGetOptions) (
			resp azfake.Responder[armnetwork.InterfacesClientGetResponse], errResp azfake.ErrorResponder) {
			w.mu.Lock()
			defer w.mu.Unlock()
			nic, ok := w.nics[name]
			if !ok {
				errResp.SetResponseError(http.StatusNotFound, "NotFound")
				return
			}
			resp.SetResponse(http.StatusOK, armnetwork.InterfacesClientGetResponse{Interface: nic}, nil)
			return
		},
		BeginDelete: func(_ context.Context, _, name string, _ *armnetwork.InterfacesClientBeginDeleteOptions) (
			resp azfake.PollerResponder[armnetwork.InterfacesClientDeleteResponse], errResp azfake.ErrorResponder) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.deletedNICs = append(w.deletedNICs, name)
			delete(w.nics, name)
			resp.SetTerminalResponse(http.StatusOK, armnetwork.InterfacesClientDeleteResponse{}, nil)
			return
		},
	}
	cred := &azfake.TokenCredential{}
	vms, err := armcompute.NewVirtualMachinesClient(cloud.SubscriptionID, cred, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{Transport: computefake.NewVirtualMachinesServerTransport(&vmServer)}})
	if err != nil {
		t.Fatal(err)
	}
	nics, err := armnetwork.NewInterfacesClient(cloud.SubscriptionID, cred, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{Transport: networkfake.NewInterfacesServerTransport(&nicServer)}})
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{cloud: cloud, shape: shape, instance: "ci", pool: "build", vms: vms, nics: nics,
		now: func() time.Time { return now }}
	return p, w
}

var shape = Shape{Size: "Standard_D4ads_v5",
	Image: "/subscriptions/s/resourceGroups/g/providers/Microsoft.Compute/galleries/g/images/runner/versions/1.0.0"}

func create(t *testing.T, p *Provider, name string) error {
	t.Helper()
	return p.Create(context.Background(), provider.CreateSpec{Name: name, JITConfig: "jit-" + name,
		ExpiresAt: now.Add(3 * time.Hour)})
}

func TestCreateBuildsAPrivateVMFromTheGalleryImage(t *testing.T) {
	s := shape
	s.EphemeralOSDisk = "ResourceDisk"
	s.Identity = "/subscriptions/s/resourceGroups/g/providers/Microsoft.ManagedIdentity/userAssignedIdentities/build"
	s.CustomData = []byte("#cloud-config\n")
	p, w := newProvider(t, s)
	if err := create(t, p, "ci-build-a"); err != nil {
		t.Fatal(err)
	}
	nic := w.nics["ci-build-a-nic"]
	ip := nic.Properties.IPConfigurations[0].Properties
	if ip.PublicIPAddress != nil || *ip.Subnet.ID != cloud.SubnetID || *nic.Tags[TagPool] != "build" {
		t.Errorf("network interface %+v", ip)
	}
	vm := w.created[0]
	props := vm.Properties
	disk := props.StorageProfile.OSDisk
	user, _ := base64.StdEncoding.DecodeString(*props.UserData)
	custom, _ := base64.StdEncoding.DecodeString(*props.OSProfile.CustomData)
	switch {
	case *props.StorageProfile.ImageReference.ID != s.Image:
		t.Error("image")
	case *disk.DiffDiskSettings.Placement != armcompute.DiffDiskPlacementResourceDisk ||
		*disk.Caching != armcompute.CachingTypesReadOnly || *disk.DeleteOption != armcompute.DiskDeleteOptionTypesDelete:
		t.Errorf("OS disk %+v", disk)
	case vm.Identity == nil || vm.Identity.UserAssignedIdentities[s.Identity] == nil:
		t.Error("identity")
	case string(user) != "jit-ci-build-a" || string(custom) != "#cloud-config\n":
		t.Errorf("user data %q, custom data %q", user, custom)
	case *props.NetworkProfile.NetworkInterfaces[0].Properties.DeleteOption != armcompute.DeleteOptionsDelete:
		t.Error("network interface not deleted with the VM")
	case props.Priority != nil:
		t.Error("priority set without Spot")
	case *vm.Tags[TagInstance] != "ci" || *vm.Tags[TagExpiresAt] != "2026-11-03T15:00:00Z" || *vm.Tags[TagPriority] != "regular":
		t.Errorf("tags %v", vm.Tags)
	case !strings.HasPrefix(*props.OSProfile.LinuxConfiguration.SSH.PublicKeys[0].KeyData, "ssh-ed25519 "):
		t.Error("ssh key")
	}
}

func TestCreateWithoutIdentityOrEphemeralDisk(t *testing.T) {
	p, w := newProvider(t, shape)
	if err := create(t, p, "ci-build-a"); err != nil {
		t.Fatal(err)
	}
	vm := w.created[0]
	if vm.Identity != nil || vm.Properties.StorageProfile.OSDisk.DiffDiskSettings != nil {
		t.Errorf("identity %+v, disk %+v", vm.Identity, vm.Properties.StorageProfile.OSDisk)
	}
}

func TestCapacityRefusalIsRetryLaterAndCleansUp(t *testing.T) {
	p, w := newProvider(t, shape)
	w.createErr = []string{"QuotaExceeded"}
	err := create(t, p, "ci-build-a")
	if !errors.Is(err, provider.ErrCapacity) {
		t.Fatalf("error %v, want ErrCapacity", err)
	}
	if len(w.nics) != 0 || len(w.deletedNICs) != 1 {
		t.Errorf("network interface left behind: %v", w.nics)
	}
	w.createErr = []string{"InvalidParameter"}
	if err := create(t, p, "ci-build-b"); err == nil || errors.Is(err, provider.ErrCapacity) {
		t.Errorf("error %v, want a plain failure", err)
	}
}

func TestAllocationFailureAfterAcceptanceIsRetryLater(t *testing.T) {
	p, w := newProvider(t, shape)
	w.allocErr = []string{"AllocationFailed"}
	if err := create(t, p, "ci-build-a"); !errors.Is(err, provider.ErrCapacity) {
		t.Fatalf("error %v, want ErrCapacity", err)
	}
}

func TestSpotFallsBackToRegular(t *testing.T) {
	s := shape
	s.Spot = true
	p, w := newProvider(t, s)
	w.allocErr = []string{"SkuNotAvailable"}
	err := create(t, p, "ci-build-a")
	if err == nil || errors.Is(err, provider.ErrCapacity) {
		t.Fatalf("error %v: want a failure that does not pause the pool", err)
	}
	if *w.created[0].Properties.Priority != armcompute.VirtualMachinePriorityTypesSpot ||
		*w.created[0].Properties.EvictionPolicy != armcompute.VirtualMachineEvictionPolicyTypesDelete {
		t.Error("first attempt was not Spot")
	}
	if err := create(t, p, "ci-build-b"); err != nil {
		t.Fatal(err)
	}
	if w.created[1].Properties.Priority != nil || *w.created[1].Tags[TagPriority] != "regular" {
		t.Error("second attempt was not regular priority")
	}
	p.now = func() time.Time { return now.Add(spotRest) }
	if err := create(t, p, "ci-build-c"); err != nil {
		t.Fatal(err)
	}
	if w.created[2].Properties.Priority == nil {
		t.Error("Spot not tried again after the rest")
	}
}

func TestListMapsStatesAndLeftovers(t *testing.T) {
	p, w := newProvider(t, shape)
	for _, name := range []string{"ci-build-run", "ci-build-off", "ci-build-dealloc"} {
		if err := create(t, p, name); err != nil {
			t.Fatal(err)
		}
	}
	w.views["ci-build-run"] = "PowerState/running"
	w.views["ci-build-off"] = "PowerState/stopped"
	w.views["ci-build-dealloc"] = "PowerState/deallocated"
	failed := w.vms["ci-build-run"]
	failed.Name = new("ci-build-failed")
	failed.Properties = &armcompute.VirtualMachineProperties{ProvisioningState: new("Failed")}
	w.vms["ci-build-failed"] = failed
	foreign := w.vms["ci-build-run"]
	foreign.Name = new("other-vm")
	foreign.Tags = map[string]*string{TagInstance: new("someone-else"), TagPool: new("build")}
	w.vms["other-vm"] = foreign
	// network interfaces whose VM never appeared: one just made, one old
	w.nics["ci-build-young-nic"] = armnetwork.Interface{Name: new("ci-build-young-nic"), Tags: p.tags(now.Add(-time.Minute), now, false)}
	w.nics["ci-build-old-nic"] = armnetwork.Interface{Name: new("ci-build-old-nic"), Tags: p.tags(now.Add(-time.Hour), now, false)}

	got, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]plan.MachineState{}
	for _, m := range got {
		states[m.Name] = m.State
		if m.Name == "ci-build-run" && !m.ExpiresAt.Equal(now.Add(3*time.Hour)) {
			t.Errorf("expiry %v", m.ExpiresAt)
		}
	}
	want := map[string]plan.MachineState{
		"ci-build-run": plan.Running, "ci-build-off": plan.Stopped, "ci-build-dealloc": plan.Stopped,
		"ci-build-failed": plan.Failed, "ci-build-young": plan.Pending, "ci-build-old": plan.Failed,
	}
	if len(states) != len(want) {
		t.Errorf("machines %v, want %v", states, want)
	}
	for name, s := range want {
		if states[name] != s {
			t.Errorf("%s: %s, want %s", name, states[name], s)
		}
	}
}

func TestDelete(t *testing.T) {
	p, w := newProvider(t, shape)
	if err := create(t, p, "ci-build-a"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(context.Background(), "ci-build-a"); err != nil || len(w.deletedVMs) != 1 {
		t.Fatalf("VM deletion: %v %v", err, w.deletedVMs)
	}
	// The VM is gone but its network interface is left (the fake does not cascade): it goes next.
	if err := p.Delete(context.Background(), "ci-build-a"); err != nil || len(w.deletedNICs) != 1 {
		t.Fatalf("network interface deletion: %v %v", err, w.deletedNICs)
	}
	if err := p.Delete(context.Background(), "ci-build-a"); err != nil {
		t.Errorf("deleting what is gone: %v", err)
	}
	w.vms["other"] = armcompute.VirtualMachine{Name: new("other"), Tags: map[string]*string{TagInstance: new("x")}}
	if err := p.Delete(context.Background(), "other"); err == nil {
		t.Error("deleted a VM of another installation")
	}
}
