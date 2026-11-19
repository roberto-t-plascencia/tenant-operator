package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsolationMode controls how a tenant's namespace is isolated on the network.
// +kubebuilder:validation:Enum=Strict;None
type IsolationMode string

const (
	// IsolationStrict denies ingress from outside the tenant namespace.
	IsolationStrict IsolationMode = "Strict"
	// IsolationNone leaves the namespace open to cluster traffic.
	IsolationNone IsolationMode = "None"
)

// TenantPhase is a coarse summary of where a tenant is in its lifecycle.
type TenantPhase string

const (
	PhaseProvisioning TenantPhase = "Provisioning"
	PhaseReady        TenantPhase = "Ready"
	PhaseFailed       TenantPhase = "Failed"
	PhaseTerminating  TenantPhase = "Terminating"
)

// TenantQuota caps the compute a tenant can request inside its namespace.
type TenantQuota struct {
	// CPU caps the sum of CPU requests in the tenant namespace.
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`

	// Memory caps the sum of memory requests in the tenant namespace.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`

	// Pods caps the number of pods in the tenant namespace.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Pods *int32 `json:"pods,omitempty"`
}

// TenantSpec is the desired state of a Tenant.
type TenantSpec struct {
	// DisplayName is a human-readable name for the tenant.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Owners are user names granted the built-in "admin" ClusterRole
	// inside the tenant namespace.
	// +optional
	Owners []string `json:"owners,omitempty"`

	// Quota caps the compute the tenant can request. No quota is applied when omitted.
	// +optional
	Quota *TenantQuota `json:"quota,omitempty"`

	// Isolation controls network isolation for the tenant namespace.
	// +kubebuilder:default=Strict
	// +optional
	Isolation IsolationMode `json:"isolation,omitempty"`
}

// TenantStatus is the observed state of a Tenant.
type TenantStatus struct {
	// ObservedGeneration is the most recent generation the controller acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Namespace is the namespace provisioned for this tenant.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Phase is a coarse summary of the tenant's lifecycle.
	// +optional
	Phase TenantPhase `json:"phase,omitempty"`

	// Conditions describe the tenant's current state in detail.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Tenant is a cluster-scoped request for an isolated, quota-bound namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 56",message="name must be a DNS label of at most 56 characters so the tenant- namespace name stays valid"
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// TenantList contains a list of Tenants.
// +kubebuilder:object:root=true
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}
