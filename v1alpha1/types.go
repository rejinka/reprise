// Package v1alpha1 defines the public RepriseRequest API.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var SchemeGroupVersion = schema.GroupVersion{Group: "reprise.naji-dev.de", Version: "v1alpha1"}

type Target struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

type Patch struct {
	Type  string         `json:"type"`
	Value map[string]any `json:"value"`
}

type Mutation struct {
	Target Target `json:"target"`
	Patch  Patch  `json:"patch"`
}

type RepriseRequestSpec struct {
	Mutations []Mutation `json:"mutations"`
}

type RecoveryField struct {
	Path           string `json:"path"`
	Existed        bool   `json:"existed"`
	ValueJSON      string `json:"valueJSON,omitempty"`
	ExpectedExists bool   `json:"expectedExists"`
	ExpectedJSON   string `json:"expectedJSON,omitempty"`
}

type RecoveryEntry struct {
	Target    Target                `json:"target"`
	UID       types.UID             `json:"uid"`
	Fields    []RecoveryField       `json:"fields"`
	ScaleZero bool                  `json:"scaleZero,omitempty"`
	Selector  *metav1.LabelSelector `json:"selector,omitempty"`
}

type RepriseRequestStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	SpecJSON           string             `json:"specJSON,omitempty"`
	Journal            []RecoveryEntry    `json:"journal,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type RepriseRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RepriseRequestSpec   `json:"spec"`
	Status            RepriseRequestStatus `json:"status,omitempty"`
}

type RepriseRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepriseRequest `json:"items"`
}

func (in *RepriseRequest) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(RepriseRequest)
	*out = *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Mutations = make([]Mutation, len(in.Spec.Mutations))
	for i, mutation := range in.Spec.Mutations {
		out.Spec.Mutations[i] = mutation
		if mutation.Patch.Value != nil {
			out.Spec.Mutations[i].Patch.Value = runtime.DeepCopyJSON(mutation.Patch.Value)
		}
	}
	out.Status.Journal = make([]RecoveryEntry, len(in.Status.Journal))
	for i, entry := range in.Status.Journal {
		out.Status.Journal[i] = entry
		out.Status.Journal[i].Fields = append([]RecoveryField(nil), entry.Fields...)
		if entry.Selector != nil {
			out.Status.Journal[i].Selector = entry.Selector.DeepCopy()
		}
	}
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	return out
}

func (in *RepriseRequestList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(RepriseRequestList)
	*out = *in
	out.ListMeta = in.ListMeta
	out.Items = make([]RepriseRequest, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*RepriseRequest))
	}
	return out
}

func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &RepriseRequest{}, &RepriseRequestList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
