// Package v1alpha1 contains the v1alpha1 API for the tenancy.plascencia.dev group.
// +kubebuilder:object:generate=true
// +groupName=tenancy.plascencia.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "tenancy.plascencia.dev", Version: "v1alpha1"}

	// SchemeBuilder adds the types in this package to a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
