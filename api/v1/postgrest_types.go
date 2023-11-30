/*
Copyright 2023.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// PostgREST properties
type PostgrestSpec struct {
	// Schema for this PostgREST instance
	// +operator-sdk:csv:customresourcedefinitions:type=spec
	Schema string `json:"schema,omitempty"` // PGRST_DB_SCHEMAS

	// Tables to expose (only if anonymous role is auto-generated)
	// +operator-sdk:csv:customresourcedefinitions:type=spec
	Tables []string `json:"tables,omitempty"`

	// Comma-separated string of permitted actions (only if anonymous role is auto-generated)
	// +operator-sdk:csv:customresourcedefinitions:type=spec
	Grants string `json:"grants,omitempty"`

	// Role used by PostgREST to authenticate on the database; if not specified, it will be auto-generated as <CR name>_postgrest_role'
	// +operator-sdk:csv:customresourcedefinitions:type=spec
	AnonRole string `json:"anonRole,omitempty"` // PGRST_DB_ANON_ROLE
}

// PostgREST status
type PostgrestStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status
	State string `json:"state,omitempty" patchStrategy:"merge"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Schema for the postgrests API
type Postgrest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgrestSpec   `json:"spec,omitempty"`
	Status PostgrestStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// List of Postgrest
type PostgrestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Postgrest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Postgrest{}, &PostgrestList{})
}
