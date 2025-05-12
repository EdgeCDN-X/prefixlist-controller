/*
Copyright 2025.

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

package v1alpha1

import (
	"fmt"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type V4Prefix struct {
	// +kubebuilder:validation:Format=ipv4
	Address string `json:"address"`
	// +kubebuilder:validation:Maximum=32
	// +kubebuilder:validation:Minimum=0
	Size int `json:"size"`
}

type V6Prefix struct {
	// +kubebuilder:validation:Format=ipv6
	Address string `json:"address"`
	// +kubebuilder:validation:Maximum=128
	// +kubebuilder:validation:Minimum=0
	Size int `json:"size"`
}

func (p V6Prefix) ToBroadcast() (uint64, error) {
	_, ipNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", p.Address, p.Size))
	if err != nil {
		return 0, err
	}
	broadcastUint1 := uint64(ipNet.IP[0])<<24 |
		uint64(ipNet.IP[1])<<16 |
		uint64(ipNet.IP[2])<<8 |
		uint64(ipNet.IP[3])
	broadcastUint1 = broadcastUint1 >> (128 - uint64(p.Size))
	broadcastUint1 = broadcastUint1 << (128 - uint64(p.Size))
	broadcastUint1 |= 1<<(32-uint64(p.Size)) - 1
	return broadcastUint1, nil
}

type Prefix struct {
	// IPv4 Prefixes
	V4 []V4Prefix `json:"v4,omitempty"`
	// IPv6 Prefixes
	V6 []V6Prefix `json:"v6,omitempty"`
}

// PrefixListSpec defines the desired state of PrefixList.
type PrefixListSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Source is the source of the prefix list. Either static of Bgp
	// +kubebuilder:validation:Enum=Static;Bgp;Controller
	Source string `json:"source"`
	// Prefix definitions
	Prefix Prefix `json:"prefix"`
	// Where to route the requests coming from this prefix list
	// +kubebuilder:validation:Required
	Destination string `json:"destination"`
}

// PrefixListStatus defines the observed state of PrefixList.
type PrefixListStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Describes the state of consolidation. The consolidated state is saved in status
	// +kubebuilder:validation:Enum=Healthy;Progressing;Degraded
	Status string `json:"status,omitempty"`
	// +kubebuilder:validation:Enum=Consolidating;Consolidated;Requested
	ConsoliadtionStatus string `json:"consolidationStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PrefixList is the Schema for the prefixlists API.
// +kubebuilder:printcolumn:name="Source",type="string",JSONPath=".spec.source"
// +kubebuilder:printcolumn:name="Destination",type="string",JSONPath=".spec.destination"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.status"
// +kubebuilder:printcolumn:name="Consolidation Status",type="string",JSONPath=".status.consolidationStatus"
type PrefixList struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PrefixListSpec   `json:"spec,omitempty"`
	Status PrefixListStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PrefixListList contains a list of PrefixList.
type PrefixListList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PrefixList `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PrefixList{}, &PrefixListList{})
}
