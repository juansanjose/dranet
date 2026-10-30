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
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// The metadata file has to match what a KEP-5304 consumer (KubeVirt) reads:
// resourceclaims/<claim>/<request>/<driver>-metadata.json with the pciBusID
// attribute of the allocated device.
func TestWriteDRADeviceMetadata(t *testing.T) {
	root := t.TempDir()
	prev := draMetadataBasePath
	draMetadataBasePath = root
	t.Cleanup(func() { draMetadataBasePath = prev })

	path, err := writeDRADeviceMetadata("dra.net", "kd-w1", "vfio-nic", "vfio", "pci-0000-01-00-0", "0000:01:00.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(root, "resourceclaims", "vfio-nic", "vfio", "dra.net-metadata.json")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading metadata: %v", err)
	}
	var md deviceMetadata
	if err := json.Unmarshal(data, &md); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if md.APIVersion != draMetadataAPIVersion || md.Kind != draMetadataKind {
		t.Fatalf("apiVersion/kind = %q/%q", md.APIVersion, md.Kind)
	}
	if len(md.Requests) != 1 || md.Requests[0].Name != "vfio" {
		t.Fatalf("requests = %#v", md.Requests)
	}
	dev := md.Requests[0].Devices[0]
	if dev.Driver != "dra.net" || dev.Pool != "kd-w1" || dev.Name != "pci-0000-01-00-0" {
		t.Fatalf("device identity = %#v", dev)
	}
	attr := dev.Attributes[draPCIBusIDAttribute]
	if attr.StringValue == nil || *attr.StringValue != "0000:01:00.0" {
		t.Fatalf("pciBusID attribute = %#v", attr)
	}
}

// createContainer must bind-mount the metadata file at the same path inside the
// container, which is where KubeVirt looks for it.
func TestCreateContainerMountsDRAMetadata(t *testing.T) {
	np := &NetworkDriver{}
	metaPath := "/var/run/kubernetes.io/dra-device-attributes/resourceclaims/vfio-nic/vfio/dra.net-metadata.json"
	podConfig := PodConfig{DeviceConfigs: map[string]DeviceConfig{
		"pci-0000-01-00-0": {VFIODevice: &VFIODeviceConfig{
			PCIAddress:   "0000:01:00.0",
			IOMMUGroup:   10,
			RequestName:  "vfio",
			MetadataPath: metaPath,
		}},
	}}

	adjust, _, err := np.createContainer(t.Context(), nil, nil, podConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(adjust.Mounts) != 1 {
		t.Fatalf("expected exactly one mount, got %#v", adjust.Mounts)
	}
	m := adjust.Mounts[0]
	if m.Source != metaPath || m.Destination != metaPath {
		t.Fatalf("mount = %#v", m)
	}
	if m.Type != "bind" {
		t.Fatalf("mount type = %q, want bind", m.Type)
	}
}

// A request with count > 1 publishes several devices into the same file: the
// second prepare must not drop the first.
func TestWriteDRADeviceMetadataMergesDevices(t *testing.T) {
	root := t.TempDir()
	prev := draMetadataBasePath
	draMetadataBasePath = root
	t.Cleanup(func() { draMetadataBasePath = prev })

	path, err := writeDRADeviceMetadata("dra.net", "kd-w1", "multi", "vfio", "pci-0000-01-00-0", "0000:01:00.0")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := writeDRADeviceMetadata("dra.net", "kd-w1", "multi", "vfio", "pci-0000-01-00-1", "0000:01:00.1"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	// Re-preparing the first device must not duplicate it.
	if _, err := writeDRADeviceMetadata("dra.net", "kd-w1", "multi", "vfio", "pci-0000-01-00-0", "0000:01:00.0"); err != nil {
		t.Fatalf("re-write: %v", err)
	}

	md, err := readDeviceMetadata(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(md.Requests) != 1 || md.Requests[0].Name != "vfio" {
		t.Fatalf("requests = %#v", md.Requests)
	}
	if got := len(md.Requests[0].Devices); got != 2 {
		t.Fatalf("got %d devices, want 2: %#v", got, md.Requests[0].Devices)
	}
	seen := map[string]string{}
	for _, d := range md.Requests[0].Devices {
		seen[d.Name] = *d.Attributes[draPCIBusIDAttribute].StringValue
	}
	if seen["pci-0000-01-00-0"] != "0000:01:00.0" || seen["pci-0000-01-00-1"] != "0000:01:00.1" {
		t.Fatalf("unexpected device set: %#v", seen)
	}
}

// Two devices of one claim/request share a metadata path, so the mount must be
// added once, not once per device.
func TestCreateContainerDeduplicatesMetadataMounts(t *testing.T) {
	metaPath := "/var/run/kubernetes.io/dra-device-attributes/resourceclaims/multi/vfio/dra.net-metadata.json"
	podConfig := PodConfig{DeviceConfigs: map[string]DeviceConfig{
		"pci-0000-01-00-0": {VFIODevice: &VFIODeviceConfig{PCIAddress: "0000:01:00.0", IOMMUGroup: 10, MetadataPath: metaPath}},
		"pci-0000-01-00-1": {VFIODevice: &VFIODeviceConfig{PCIAddress: "0000:01:00.1", IOMMUGroup: 11, MetadataPath: metaPath}},
	}}
	np := &NetworkDriver{}
	adjust, _, err := np.createContainer(t.Context(), nil, nil, podConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(adjust.Mounts) != 1 {
		t.Fatalf("got %d mounts, want 1: %#v", len(adjust.Mounts), adjust.Mounts)
	}
}

// After a claim is unprepared and dropped from the store, its metadata file
// must go away, while a file referenced by a surviving claim stays.
func TestSweepDRADeviceMetadata(t *testing.T) {
	root := t.TempDir()
	prev := draMetadataBasePath
	draMetadataBasePath = root
	t.Cleanup(func() { draMetadataBasePath = prev })

	released, err := writeDRADeviceMetadata("dra.net", "kd-w1", "shared", "vfio", "pci-0000-01-00-0", "0000:01:00.0")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	kept, err := writeDRADeviceMetadata("dra.net", "kd-w1", "other", "vfio", "pci-0000-02-00-0", "0000:02:00.0")
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	store, err := NewPodConfigStore("")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// The surviving claim is still referenced by a pod; the released one has
	// already been deleted from the store by unprepare.
	if err := store.SetDeviceConfig(types.UID("pod-b"), "pci-0000-02-00-0", DeviceConfig{
		Claim:      types.NamespacedName{Namespace: "default", Name: "other"},
		VFIODevice: &VFIODeviceConfig{MetadataPath: kept},
	}); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}

	np := &NetworkDriver{podConfigStore: store}
	np.sweepDRADeviceMetadata("shared")

	if _, err := os.Stat(released); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("released claim's metadata still exists: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("surviving claim's metadata was removed: %v", err)
	}
}
