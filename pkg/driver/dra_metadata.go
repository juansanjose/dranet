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
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// writeDRADeviceMetadata writes the KEP-5304 metadata file for one allocated
// device and returns its path. Consumers mount that path into the container.
// The file is written atomically so a concurrent reader never sees a partial
// document.
func writeDRADeviceMetadata(driverName, pool, claimName, requestName, deviceName, pciAddress string) (string, error) {
	if claimName == "" || requestName == "" {
		return "", fmt.Errorf("cannot write DRA device metadata without claim and request names")
	}
	pci := pciAddress
	md := deviceMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: draMetadataAPIVersion,
			Kind:       draMetadataKind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: claimName},
		Requests: []deviceMetadataRequest{{
			Name: requestName,
			Devices: []deviceMetadataDevice{{
				Driver: driverName,
				Pool:   pool,
				Name:   deviceName,
				Attributes: map[string]deviceAttribute{
					draPCIBusIDAttribute: {StringValue: &pci},
				},
			}},
		}},
	}

	path := draMetadataPath(claimName, requestName, driverName)
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
