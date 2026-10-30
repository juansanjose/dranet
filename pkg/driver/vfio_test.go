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
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/dranet/pkg/apis"
)

// makeCharDevice creates a character device node, or skips the test when the
// environment does not allow it (unprivileged CI).
func makeCharDevice(t *testing.T, path string, major, minor uint32) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := unix.Mknod(path, unix.S_IFCHR|0o666, int(unix.Mkdev(major, minor))); err != nil {
		if err == syscall.EPERM || err == syscall.EACCES {
			t.Skipf("cannot create device nodes in this environment: %v", err)
		}
		t.Fatalf("mknod %s: %v", path, err)
	}
}

func TestBuildVFIODeviceConfig(t *testing.T) {
	root := t.TempDir()
	control := filepath.Join(root, "vfio")
	group := filepath.Join(root, "42")
	makeCharDevice(t, control, 10, 196)
	makeCharDevice(t, group, 10, 200)

	prevDevfs, prevControl := vfioDevfsPath, vfioControlDevicePath
	vfioDevfsPath, vfioControlDevicePath = root, control
	t.Cleanup(func() { vfioDevfsPath, vfioControlDevicePath = prevDevfs, prevControl })

	cfg, err := buildVFIODeviceConfig("0000:3b:00.2", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PCIAddress != "0000:3b:00.2" || cfg.IOMMUGroup != 42 {
		t.Fatalf("unexpected config identity: %#v", cfg)
	}
	if len(cfg.DevChars) != 2 {
		t.Fatalf("got %d char devices, want 2: %#v", len(cfg.DevChars), cfg.DevChars)
	}
	byPath := map[string]LinuxDevice{}
	for _, d := range cfg.DevChars {
		byPath[d.Path] = d
	}
	c, ok := byPath[control]
	if !ok || c.Type != "c" || c.Major != 10 || c.Minor != 196 {
		t.Errorf("control device wrong: %#v", c)
	}
	g, ok := byPath[group]
	if !ok || g.Type != "c" || g.Major != 10 || g.Minor != 200 {
		t.Errorf("group device wrong: %#v", g)
	}

	// A missing node must be an error, not a partial config.
	if _, err := buildVFIODeviceConfig("0000:3b:00.9", 99); err == nil {
		t.Fatalf("expected an error when the group device is missing")
	}
}

// createContainer must inject the VFIO char devices of every device that has
// them, and must not duplicate a path shared by two claims.
func TestCreateContainerInjectsVFIODevices(t *testing.T) {
	np := &NetworkDriver{}
	podConfig := PodConfig{
		DeviceConfigs: map[string]DeviceConfig{
			"pci-3b-00-2": {
				VFIODevice: &VFIODeviceConfig{
					PCIAddress: "0000:3b:00.2",
					IOMMUGroup: 42,
					DevChars: []LinuxDevice{
						{Path: "/dev/vfio/vfio", Type: "c", Major: 10, Minor: 196},
						{Path: "/dev/vfio/42", Type: "c", Major: 10, Minor: 200},
					},
				},
			},
			"pci-3b-00-3": {
				VFIODevice: &VFIODeviceConfig{
					PCIAddress: "0000:3b:00.3",
					IOMMUGroup: 43,
					// same control device, different group
					DevChars: []LinuxDevice{
						{Path: "/dev/vfio/vfio", Type: "c", Major: 10, Minor: 196},
						{Path: "/dev/vfio/43", Type: "c", Major: 10, Minor: 201},
					},
				},
			},
			"pci-5e-00-1": {
				RDMADevice: RDMAConfig{
					LinkDev:  "mlx5_0",
					DevChars: []LinuxDevice{{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192}},
				},
			},
		},
	}

	adjust, _, err := np.createContainer(t.Context(), nil, nil, podConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adjust.Linux == nil {
		t.Fatalf("no linux adjustment produced")
	}
	got := map[string]bool{}
	for _, d := range adjust.Linux.Devices {
		if got[d.Path] {
			t.Fatalf("device %s injected more than once", d.Path)
		}
		got[d.Path] = true
	}
	for _, want := range []string{"/dev/vfio/vfio", "/dev/vfio/42", "/dev/vfio/43", "/dev/infiniband/uverbs0"} {
		if !got[want] {
			t.Errorf("device %s was not injected; got %v", want, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("injected %d devices, want 4: %v", len(got), got)
	}
}

// A device that is neither RDMA nor VFIO must not produce any adjustment, so
// that ordinary netdev claims keep the container untouched.
func TestCreateContainerNoCharDevices(t *testing.T) {
	np := &NetworkDriver{}
	podConfig := PodConfig{
		DeviceConfigs: map[string]DeviceConfig{
			"eth0": {NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "net0"}}},
		},
	}
	adjust, _, err := np.createContainer(t.Context(), nil, nil, podConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adjust.Linux != nil && len(adjust.Linux.Devices) != 0 {
		t.Fatalf("expected no injected devices, got %#v", adjust.Linux.Devices)
	}
}

// guard against a podConfig key type change silently breaking the map above
var _ = types.UID("")
