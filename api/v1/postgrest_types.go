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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// PostgrestSpec defines the desired state of Postgrest
type PostgrestSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec
	DatabaseUri string `json:"databaseUri,omitempty"` // PGRST_DB_URI

	// +operator-sdk:csv:customresourcedefinitions:type=spec
	Schemas string `json:"schema,omitempty"` // PGRST_DB_SCHEMAS

	// TODO tables for list of tables to expose
	// TODO flag for read/write permissions

	// +operator-sdk:csv:customresourcedefinitions:type=spec
	// if specified: check it exists, assume its permissions are already correct
	// if not specified: create with permissions as <clean CR name>_postgrest_role
	AnonRole string `json:"anonRole,omitempty"` // PGRST_DB_ANON_ROLE

	// TODO authentication -> basic/jwt
	// authentication.basic.user + authentication.basic.password
	// authentication.jwt.secret
}

// PostgrestStatus defines the observed state of Postgrest
type PostgrestStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Postgrest is the Schema for the postgrests API
type Postgrest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgrestSpec   `json:"spec,omitempty"`
	Status PostgrestStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PostgrestList contains a list of Postgrest
type PostgrestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Postgrest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Postgrest{}, &PostgrestList{})
}
