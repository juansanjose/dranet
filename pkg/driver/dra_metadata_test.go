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
	"os"
	"path/filepath"
	"testing"
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
