/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// supportedProtocol is the type alias for supported Protocol string values; the alias is defined specifically for the
// purpose of enforcing correct enums on the Protocol field of ServicePort struct.
// +kubebuilder:validation:Enum="TCP";"UDP";"SCTP"
type supportedProtocol string

// ServicePort specifies a list of ports a Service exposes.
type ServicePort struct {
	// The name of the exported port in this Service.
	// +optional
	Name string `json:"name,omitempty"`
	// The IP protocol for this exported port; its value must be one of TCP, UDP, or SCTP and it defaults to TCP.
	// +kubebuilder:default:="TCP"
	// +optional
	Protocol supportedProtocol `json:"protocol,omitempty"`
	// The application protocol for this port; this field follows standard Kubernetes label syntax.
	// Un-prefixed names are reserved for IANA standard service names (as per RFC-6335 and
	// http://www.iana.org/assignments/service-names).
	// Non-standard protocols should use prefixed names such as example.com/protocol.
	// +optional
	AppProtocol string `json:"appProtocol,omitempty"`
	// The exported port.
	// +kubebuilder:validation:Minimum:=0
	// +kubebuilder:validation:Maximum:=65535
	// +kubebuilder:validation:Required
	Port int32 `json:"port"`
	// The number or name of the target port.
	// +kubebuilder:validation:Required
	TargetPort intstr.IntOrString `json:"targetPort"`
}

// InternalServiceExportSpec specifies the spec of an exported Service; at this stage only the ports of an
// exported Service are sync'd.
type InternalServiceExportSpec struct {
	// A list of ports exposed by the exported Service.
	// +listType=atomic
	// +kubebuilder:validation:Required
	Ports []ServicePort `json:"ports"`
}

// InternalServiceExportStatus contains the current status of an InternalServiceExport.
type InternalServiceExportStatus struct {
	// +optional
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

//+kubebuilder:object:root=true

// InternalServiceExport is a data transport type that member clusters in the fleet use to upload the spec of
// exported Service to the hub cluster.
type InternalServiceExport struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	Spec InternalServiceExportSpec `json:"spec,omitempty"`
	// +optional
	Status InternalServiceExportStatus `json:"status,omitempty"`
}

// InternalServiceExportList contains a list of InternalServiceExports.
type InternalServiceExportList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	// +listType=set
	Items []InternalServiceExport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InternalServiceExport{}, &InternalServiceExportList{})
}