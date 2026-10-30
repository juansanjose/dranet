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

package apis

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestValidateVFIOConfig(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantErrs    int
		wantContain string
	}{
		{
			name:     "vfio only is valid",
			raw:      `{"vfio":{}}`,
			wantErrs: 0,
		},
		{
			name:     "empty config is valid",
			raw:      `{}`,
			wantErrs: 0,
		},
		{
			name:        "interface name is rejected",
			raw:         `{"vfio":{},"interface":{"name":"net0"}}`,
			wantErrs:    1,
			wantContain: "interface configuration is not supported for VFIO devices",
		},
		{
			name:        "addresses are rejected",
			raw:         `{"vfio":{},"interface":{"addresses":["10.0.0.2/24"]}}`,
			wantErrs:    1,
			wantContain: "interface configuration is not supported",
		},
		{
			name:        "mtu is rejected",
			raw:         `{"vfio":{},"interface":{"mtu":9000}}`,
			wantErrs:    1,
			wantContain: "interface configuration is not supported",
		},
		{
			name:        "routes are rejected",
			raw:         `{"vfio":{},"routes":[{"destination":"0.0.0.0/0"}]}`,
			wantErrs:    1,
			wantContain: "routes are not supported for VFIO devices",
		},
		{
			name:        "rules are rejected",
			raw:         `{"vfio":{},"rules":[{"table":100}]}`,
			wantErrs:    1,
			wantContain: "rules are not supported for VFIO devices",
		},
		{
			name:        "neighbors are rejected",
			raw:         `{"vfio":{},"neighbors":[{"destination":"10.0.0.1","hardwareAddr":"aa:bb:cc:dd:ee:ff"}]}`,
			wantErrs:    1,
			wantContain: "neighbors are not supported for VFIO devices",
		},
		{
			name:        "ethtool is rejected",
			raw:         `{"vfio":{},"ethtool":{"features":{"rx-checksumming":true}}}`,
			wantErrs:    1,
			wantContain: "ethtool configuration is not supported for VFIO devices",
		},
		{
			name:        "profile is rejected",
			raw:         `{"vfio":{},"profile":"gce"}`,
			wantErrs:    1,
			wantContain: "profile is not supported for VFIO devices",
		},
		{
			name:        "several violations are all reported",
			raw:         `{"vfio":{},"interface":{"name":"net0"},"routes":[{"destination":"10.0.0.0/8"}],"profile":"x"}`,
			wantErrs:    3,
			wantContain: "profile is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := &runtime.RawExtension{Raw: []byte(tt.raw)}
			errs := ValidateVFIOConfig(raw)
			if len(errs) != tt.wantErrs {
				t.Fatalf("got %d errors (%v), want %d", len(errs), errs, tt.wantErrs)
			}
			if tt.wantContain != "" {
				var found bool
				for _, err := range errs {
					if strings.Contains(err.Error(), tt.wantContain) {
						found = true
					}
				}
				if !found {
					t.Fatalf("no error contains %q: %v", tt.wantContain, errs)
				}
			}
		})
	}
}

// A claim that only sets vfio must survive ValidateConfig, otherwise
// prepareDevice would reject it before ValidateVFIOConfig ever runs.
func TestValidateConfigAcceptsVFIO(t *testing.T) {
	raw := &runtime.RawExtension{Raw: []byte(`{"vfio":{}}`)}
	conf, errs := ValidateConfig(raw)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if conf == nil || conf.VFIO == nil {
		t.Fatalf("vfio config was not parsed: %#v", conf)
	}
}
