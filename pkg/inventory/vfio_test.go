/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package inventory

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jaypipes/ghw"
	"github.com/jaypipes/pcidb"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
)

// fakeSysfs builds a minimal /sys/bus/pci/devices tree and points the package
// at it for the duration of the test.
func fakeSysfs(t *testing.T, devices map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for address, group := range devices {
		dir := filepath.Join(root, address)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		link := filepath.Join(dir, "iommu_group")
		target := filepath.Join("../../../kernel/iommu_groups", strconv.Itoa(group))
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
	}
	prev := pciDevicesSysfs
	pciDevicesSysfs = root
	t.Cleanup(func() { pciDevicesSysfs = prev })
	return root
}

func TestIsPassthroughNetworkDevice(t *testing.T) {
	cases := []struct {
		driver string
		want   bool
	}{
		{driver: "vfio-pci", want: true},
		{driver: "uio_pci_generic", want: false},
		{driver: "igb_uio", want: false},
		{driver: "pci-stub", want: false},
		{driver: "mlx5_core", want: false},
		{driver: "", want: false},
	}
	for _, tc := range cases {
		dev := &ghw.PCIDevice{Driver: tc.driver}
		if got := isPassthroughNetworkDevice(dev); got != tc.want {
			t.Errorf("isPassthroughNetworkDevice(%q) = %v, want %v", tc.driver, got, tc.want)
		}
	}
}

func TestReadIOMMUGroup(t *testing.T) {
	fakeSysfs(t, map[string]int{
		"0000:3b:00.2": 42,
		"0000:5e:00.1": 7,
	})

	if got, err := readIOMMUGroup("0000:3b:00.2"); err != nil || got != 42 {
		t.Fatalf("readIOMMUGroup(0000:3b:00.2) = %d, %v; want 42, nil", got, err)
	}
	// A short address without the domain must be completed, because sysfs
	// directory names always carry it.
	if got, err := readIOMMUGroup("5e:00.1"); err != nil || got != 7 {
		t.Fatalf("readIOMMUGroup(5e:00.1) = %d, %v; want 7, nil", got, err)
	}
	if _, err := readIOMMUGroup("0000:aa:00.0"); err == nil {
		t.Fatalf("expected an error for a PCI address with no sysfs entry")
	}
	if _, err := readIOMMUGroup(""); err == nil {
		t.Fatalf("expected an error for an empty PCI address")
	}
}

func TestSysfsPCIAddress(t *testing.T) {
	cases := map[string]string{
		"3b:00.2":      "0000:3b:00.2",
		"0000:3b:00.2": "0000:3b:00.2",
		"0001:3b:00.2": "0001:3b:00.2",
	}
	for in, want := range cases {
		if got := sysfsPCIAddress(in); got != want {
			t.Errorf("sysfsPCIAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// discoverPCIDevices must publish a network function bound to vfio-pci with the
// driver and IOMMU group attributes a claim needs, while still rejecting
// functions bound to drivers that cannot be handed to a workload.
func TestDiscoverPCIDevicesPublishesVFIO(t *testing.T) {
	fakeSysfs(t, map[string]int{"0000:3b:00.2": 42})

	pciInfo := &ghw.PCIInfo{
		Devices: []*ghw.PCIDevice{
			{Address: "0000:3b:00.2", Driver: "vfio-pci", Class: &pcidb.Class{ID: pciBaseClassNetwork}},
			{Address: "0000:3b:00.3", Driver: "uio_pci_generic", Class: &pcidb.Class{ID: pciBaseClassNetwork}},
			{Address: "0000:3b:00.4", Driver: "ice", Class: &pcidb.Class{ID: pciBaseClassNetwork}},
		},
	}

	db := &DB{}
	devices := db.discoverPCIDevices(pciInfo)
	if len(devices) != 2 {
		t.Fatalf("got %d published devices, want 2 (vfio-pci + ice): %#v", len(devices), devices)
	}

	var vfio, netdev *hostDevice
	for i := range devices {
		drv := devices[i].Attributes[apis.AttrDriver].StringValue
		switch {
		case drv != nil && *drv == "vfio-pci":
			vfio = &devices[i]
		case drv != nil && *drv == "ice":
			netdev = &devices[i]
		}
	}
	if vfio == nil {
		t.Fatalf("vfio-pci device was not published: %#v", devices)
	}
	if v := vfio.Attributes[apis.AttrIOMMUGroup].IntValue; v == nil || *v != 42 {
		t.Fatalf("iommuGroup attribute = %v, want 42", v)
	}
	if v := vfio.Attributes[apis.AttrPCIAddress].StringValue; v == nil || *v != "0000:3b:00.2" {
		t.Fatalf("pciAddress attribute = %v", v)
	}
	if netdev == nil {
		t.Fatalf("the netdev-backed device was not published: %#v", devices)
	}
	if _, ok := netdev.Attributes[apis.AttrIOMMUGroup]; ok {
		t.Fatalf("a netdev device must not carry an iommuGroup attribute")
	}
}

// A vfio-pci function without a resolvable IOMMU group cannot be handed to a
// workload, so it must not be published at all.
func TestDiscoverPCIDevicesSkipsVFIOWithoutIOMMUGroup(t *testing.T) {
	fakeSysfs(t, map[string]int{})

	pciInfo := &ghw.PCIInfo{Devices: []*ghw.PCIDevice{{
		Address: "0000:3b:00.2",
		Driver:  "vfio-pci",
		Class:   &pcidb.Class{ID: pciBaseClassNetwork},
	}}}
	db := &DB{}
	if devices := db.discoverPCIDevices(pciInfo); len(devices) != 0 {
		t.Fatalf("expected no published devices, got %#v", devices)
	}
}

func vfioTestDevice(pciAddress, driver string) resourceapi.Device {
	return resourceapi.Device{
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			apis.AttrPCIAddress: {StringValue: ptr.To(pciAddress)},
			apis.AttrDriver:     {StringValue: ptr.To(driver)},
		},
	}
}

func TestGetVFIODeviceInfo(t *testing.T) {
	fakeSysfs(t, map[string]int{"0000:3b:00.2": 42})

	db := &DB{deviceStore: map[string]hostDevice{
		"pci-3b-00-2": {Device: vfioTestDevice("0000:3b:00.2", "vfio-pci")},
		"pci-3b-00-3": {Device: vfioTestDevice("0000:3b:00.3", "ice")},
	}}

	addr, group, err := db.GetVFIODeviceInfo("pci-3b-00-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "0000:3b:00.2" || group != 42 {
		t.Fatalf("got (%q, %d), want (0000:3b:00.2, 42)", addr, group)
	}

	if _, _, err := db.GetVFIODeviceInfo("pci-3b-00-3"); err == nil {
		t.Fatalf("expected an error for a device not bound to vfio-pci")
	}
	if _, _, err := db.GetVFIODeviceInfo("does-not-exist"); err == nil {
		t.Fatalf("expected an error for an unknown device")
	}
}
