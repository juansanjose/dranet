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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// KEP-5304 defines how a DRA driver publishes per-device metadata to the
// containers it prepares: a JSON file per (claim, request, driver) that the
// consumer (a VMM such as KubeVirt, or a device plugin shim) reads from inside
// the container to learn facts that are not in the ResourceClaim, such as the
// PCI address of a passthrough function.
//
// Layout, as consumed by KubeVirt (pkg/dra/utils.go):
//
//	/var/run/kubernetes.io/dra-device-attributes/resourceclaims/<claim>/<request>/<driver>-metadata.json
const (
	draMetadataAPIVersion = "metadata.resource.k8s.io/v1alpha1"
	draMetadataKind       = "DeviceMetadata"
	// draPCIBusIDAttribute is the standard attribute a consumer looks up to get
	// the PCI address of a passthrough device.
	draPCIBusIDAttribute = "resource.kubernetes.io/pciBusID"
)

// draMetadataBasePath is the directory KEP-5304 reserves for device metadata.
// A variable so unit tests can write into a temporary tree.
var draMetadataBasePath = "/var/run/kubernetes.io/dra-device-attributes"

// draMetadataMu serialises read-modify-write cycles on the metadata files: one
// claim's devices may be prepared concurrently, and a request with count > 1
// publishes several devices into the same file.
var draMetadataMu sync.Mutex

// deviceMetadata mirrors the KEP-5304 v1alpha1 types. It is written as a JSON
// stream so that a newer version can be appended without breaking older
// consumers; DRANET writes a single object.
type deviceMetadata struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Requests          []deviceMetadataRequest `json:"requests,omitempty"`
}

type deviceMetadataRequest struct {
	Name    string                 `json:"name"`
	Devices []deviceMetadataDevice `json:"devices,omitempty"`
}

type deviceMetadataDevice struct {
	Driver     string                     `json:"driver"`
	Pool       string                     `json:"pool"`
	Name       string                     `json:"name"`
	Attributes map[string]deviceAttribute `json:"attributes,omitempty"`
}

type deviceAttribute struct {
	StringValue *string `json:"string,omitempty"`
}

// draMetadataPath returns the host and in-container path of the metadata file
// for one claim/request/driver triple. The two are the same string: the file is
// bind-mounted into the workload container at the path KEP-5304 defines.
func draMetadataPath(claimName, requestName, driverName string) string {
	return filepath.Join(draMetadataBasePath, "resourceclaims", claimName, requestName, driverName+"-metadata.json")
}

// writeDRADeviceMetadata publishes the KEP-5304 metadata for one allocated
// device and returns the file path. Consumers mount that path into the
// container.
//
// The file may already exist: a request with count > 1 allocates several
// devices and each prepare call adds one of them, and a re-prepared claim must
// not lose the devices it already had. The file is therefore merged under a lock
// and replaced atomically, so a concurrent reader never sees a partial document
// or one that dropped a device.
func writeDRADeviceMetadata(driverName, pool, claimName, requestName, deviceName, pciAddress string) (string, error) {
	if claimName == "" || requestName == "" || deviceName == "" {
		return "", fmt.Errorf("cannot write DRA device metadata without claim, request and device names")
	}

	draMetadataMu.Lock()
	defer draMetadataMu.Unlock()

	path := draMetadataPath(claimName, requestName, driverName)
	md := deviceMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: draMetadataAPIVersion,
			Kind:       draMetadataKind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: claimName},
	}
	if existing, err := readDeviceMetadata(path); err == nil {
		md = *existing
		// Normalise the version fields even if the file carried more objects.
		md.APIVersion = draMetadataAPIVersion
		md.Kind = draMetadataKind
		md.Name = claimName
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("failed to read existing metadata %s: %w", path, err)
	}

	device := deviceMetadataDevice{Driver: driverName, Pool: pool, Name: deviceName}
	if pciAddress != "" {
		pci := pciAddress
		device.Attributes = map[string]deviceAttribute{draPCIBusIDAttribute: {StringValue: &pci}}
	}
	md.Requests = upsertMetadataDevice(md.Requests, requestName, device)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("failed to create metadata directory for %s: %w", path, err)
	}
	data, err := json.Marshal(md)
	if err != nil {
		return "", fmt.Errorf("failed to marshal DRA device metadata: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", fmt.Errorf("failed to write DRA device metadata %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("failed to publish DRA device metadata %s: %w", path, err)
	}
	return path, nil
}

// upsertMetadataDevice adds or replaces one device inside the request with the
// given name, creating the request if needed. It is idempotent because prepare
// can be called again for the same device.
func upsertMetadataDevice(requests []deviceMetadataRequest, requestName string, device deviceMetadataDevice) []deviceMetadataRequest {
	for i := range requests {
		if requests[i].Name != requestName {
			continue
		}
		for j := range requests[i].Devices {
			if requests[i].Devices[j].Name == device.Name {
				requests[i].Devices[j] = device
				return requests
			}
		}
		requests[i].Devices = append(requests[i].Devices, device)
		return requests
	}
	return append(requests, deviceMetadataRequest{Name: requestName, Devices: []deviceMetadataDevice{device}})
}

func readDeviceMetadata(path string) (*deviceMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// The file is a JSON stream; consumers read the first object they
	// understand. DRANET writes exactly one.
	dec := json.NewDecoder(bytes.NewReader(data))
	var md deviceMetadata
	if err := dec.Decode(&md); err != nil {
		return nil, fmt.Errorf("failed to decode %s: %w", path, err)
	}
	return &md, nil
}

// sweepDRADeviceMetadata removes metadata files that no remaining device config
// references, after a claim has been unprepared. Files live under
// resourceclaims/<claim>/..., so only that claim's subtree is swept.
//
// The sweep runs after the store dropped the claim: deciding per device during
// the release loop would keep the file alive for every device but the last,
// because the siblings are still in the store at that point.
func (np *NetworkDriver) sweepDRADeviceMetadata(claimName string) {
	if claimName == "" {
		return
	}
	draMetadataMu.Lock()
	defer draMetadataMu.Unlock()

	live := map[string]struct{}{}
	for _, podUID := range np.podConfigStore.ListPods() {
		podCfg, ok := np.podConfigStore.GetPodConfig(podUID)
		if !ok {
			continue
		}
		for _, devCfg := range podCfg.DeviceConfigs {
			if devCfg.VFIODevice != nil && devCfg.VFIODevice.MetadataPath != "" {
				live[devCfg.VFIODevice.MetadataPath] = struct{}{}
			}
		}
	}

	claimDir := filepath.Join(draMetadataBasePath, "resourceclaims", claimName)
	err := filepath.WalkDir(claimDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		if _, ok := live[path]; ok {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			klog.V(2).Infof("failed to remove DRA device metadata %s: %v", path, rmErr)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		klog.V(2).Infof("failed to sweep DRA device metadata for claim %s: %v", claimName, err)
	}
}
