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
	"testing"

	"github.com/jaypipes/ghw"

	"sigs.k8s.io/dranet/pkg/apis"
)

// TestRealVFIODeviceDiscovery is an opt-in check against the hardware of the
// machine running the test: it asserts that every PCI network function bound to
// vfio-pci is published with its IOMMU group. Set DRANET_REAL_VFIO=1 on a node
// that has at least one such function (a NIC or IB HCA bound to vfio-pci).
func TestRealVFIODeviceDiscovery(t *testing.T) {
	if os.Getenv("DRANET_REAL_VFIO") == "" {
		t.Skip("set DRANET_REAL_VFIO=1 to run against the real PCI bus")
	}
	pciInfo, err := ghw.PCI()
	if err != nil {
		t.Fatalf("ghw.PCI: %v", err)
	}

	db := &DB{}
	devices := db.discoverPCIDevices(pciInfo)

	published := 0
	for _, device := range devices {
		driver := device.Attributes[apis.AttrDriver].StringValue
		if driver == nil || *driver != vfioDriver {
			continue
		}
		published++
		addr := device.Attributes[apis.AttrPCIAddress].StringValue
		group := device.Attributes[apis.AttrIOMMUGroup].IntValue
		if addr == nil || group == nil {
			t.Fatalf("vfio device %s published without pciAddress/iommuGroup: %#v", device.Name, device.Attributes)
		}
		wantGroup, err := readIOMMUGroup(*addr)
		if err != nil {
			t.Fatalf("readIOMMUGroup(%s): %v", *addr, err)
		}
		if int64(wantGroup) != *group {
			t.Fatalf("device %s published group %d, sysfs says %d", *addr, *group, wantGroup)
		}
		t.Logf("published vfio device: name=%s addr=%s group=%d", device.Name, *addr, *group)
	}
	if published == 0 {
		t.Skip("no PCI function is bound to vfio-pci on this host")
	}
}
