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

package driver

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jaypipes/ghw"
)

// TestRealVFIOCharDevices is an opt-in check against the hardware of the
// machine running the test: for a PCI network function bound to vfio-pci it
// resolves the actual /dev/vfio nodes and asserts their type and numbers, which
// is exactly what NRI later injects into the container. Set DRANET_REAL_VFIO=1.
func TestRealVFIOCharDevices(t *testing.T) {
	if os.Getenv("DRANET_REAL_VFIO") == "" {
		t.Skip("set DRANET_REAL_VFIO=1 to run against the real PCI bus")
	}
	pciInfo, err := ghw.PCI()
	if err != nil {
		t.Fatalf("ghw.PCI: %v", err)
	}

	var addr string
	for _, dev := range pciInfo.Devices {
		if dev.Driver == "vfio-pci" && dev.Class != nil && dev.Class.ID == "02" {
			addr = dev.Address
			break
		}
	}
	if addr == "" {
		t.Skip("no PCI network function is bound to vfio-pci on this host")
	}

	link := filepath.Join(pciDevicesSysfsForTest(), addr, "iommu_group")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read %s: %v", link, err)
	}
	group, err := strconv.Atoi(filepath.Base(target))
	if err != nil {
		t.Fatalf("unexpected iommu_group link %q: %v", target, err)
	}

	cfg, err := buildVFIODeviceConfig(addr, group)
	if err != nil {
		t.Fatalf("buildVFIODeviceConfig(%s, %d): %v", addr, group, err)
	}
	if len(cfg.DevChars) != 2 {
		t.Fatalf("got %d char devices, want 2: %#v", len(cfg.DevChars), cfg.DevChars)
	}
	for _, dev := range cfg.DevChars {
		t.Logf("injected device: %s type=%s major=%d minor=%d", dev.Path, dev.Type, dev.Major, dev.Minor)
		if dev.Type != "c" {
			t.Errorf("%s is not a character device", dev.Path)
		}
	}
}

func pciDevicesSysfsForTest() string {
	return "/sys/bus/pci/devices"
}
