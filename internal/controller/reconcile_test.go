package controller

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRestoreContinuesAfterOneTargetConflicts(t *testing.T) {
	ctx := context.Background()
	request := requestObject("many", "default")
	request.SetUID(types.UID("request-uid"))
	request.SetFinalizers([]string{Finalizer})
	status := Status{Journal: []Entry{
		{Target: Target{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "safe"}, UID: types.UID("safe-uid"), Fields: []Field{{Path: "/data/mode", Existed: true, ValueJSON: `"normal"`, ExpectedExists: true, ExpectedJSON: `"paused"`}}},
		{Target: Target{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "conflict"}, UID: types.UID("conflict-uid"), Fields: []Field{{Path: "/data/mode", Existed: true, ValueJSON: `"normal"`, ExpectedExists: true, ExpectedJSON: `"paused"`}}},
	}}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	request.Object["status"] = raw
	makeTarget := func(name, uid, mode string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": name, "namespace": "default", "uid": uid,
				"annotations": map[string]any{LockAnnotation: "request-uid", AppliedAnnotation: "request-uid"}},
			"data": map[string]any{"mode": mode},
		}}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequest"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequestList"}, &unstructured.UnstructuredList{})
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, makeTarget("safe", "safe-uid", "paused"), makeTarget("conflict", "conflict-uid", "external")).WithStatusSubresource(request).Build()
	r := &Reconciler{Client: c, Mapper: mapper}
	if _, err := r.restore(ctx, request, status); err != nil {
		t.Fatal(err)
	}
	safe := &unstructured.Unstructured{}
	safe.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "safe"}, safe); err != nil {
		t.Fatal(err)
	}
	if mode, _, _ := unstructured.NestedString(safe.Object, "data", "mode"); mode != "normal" {
		t.Fatalf("safe target remains mutated: %s", mode)
	}
	if safe.GetAnnotations()[LockAnnotation] != "" || safe.GetAnnotations()[AppliedAnnotation] != "" {
		t.Fatalf("safe target remains locked: %#v", safe.GetAnnotations())
	}
	conflict := &unstructured.Unstructured{}
	conflict.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "conflict"}, conflict); err != nil {
		t.Fatal(err)
	}
	if mode, _, _ := unstructured.NestedString(conflict.Object, "data", "mode"); mode != "external" {
		t.Fatalf("conflicting value overwritten: %s", mode)
	}
	stored := requestObject("many", "default")
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "many"}, stored); err != nil {
		t.Fatal(err)
	}
	if !contains(stored.GetFinalizers(), Finalizer) {
		t.Fatal("finalizer removed despite conflict")
	}
}

func TestDeploymentScaleWaitsForPodAndRestoresReplicaNumber(t *testing.T) {
	ctx := context.Background()
	request := requestObject("scale", "default")
	request.SetUID(types.UID("scale-request"))
	request.SetGeneration(1)
	request.Object["spec"] = map[string]any{"mutations": []any{map[string]any{
		"target": map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "app"},
		"patch":  map[string]any{"type": "merge", "value": map[string]any{"spec": map[string]any{"replicas": int64(0)}}},
	}}}
	deployment := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "app", "namespace": "default", "uid": "deployment-uid", "generation": int64(2)},
		"spec":     map[string]any{"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"app": "test"}}},
		"status":   map[string]any{"observedGeneration": int64(2)},
	}}
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "app-pod", "namespace": "default", "uid": "pod-uid", "labels": map[string]any{"app": "test"}},
	}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequest"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequestList"}, &unstructured.UnstructuredList{})
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "apps", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, deployment, pod).WithStatusSubresource(request).Build()
	r := &Reconciler{Client: c, Mapper: mapper}
	key := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "scale"}}
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	stored := requestObject("scale", "default")
	if err := c.Get(ctx, key.NamespacedName, stored); err != nil {
		t.Fatal(err)
	}
	status, err := readStatus(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Journal) != 1 || status.Journal[0].Fields[0].ValueJSON != "1" {
		t.Fatalf("replica count lost in journal: %#v", status.Journal)
	}
	for _, condition := range status.Conditions {
		if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
			t.Fatal("Ready while Pod still exists")
		}
	}
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key.NamespacedName, stored); err != nil {
		t.Fatal(err)
	}
	status, err = readStatus(stored)
	if err != nil {
		t.Fatal(err)
	}
	ready := false
	for _, condition := range status.Conditions {
		if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		t.Fatalf("not Ready after Pod vanished: %#v", status.Conditions)
	}
	if err := c.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "app"}, current); err != nil {
		t.Fatal(err)
	}
	replicas, _, _ := unstructured.NestedInt64(current.Object, "spec", "replicas")
	if replicas != 1 {
		t.Fatalf("replicas not restored: %d", replicas)
	}
}

func TestFieldCaptureDistinguishesMissingAndNull(t *testing.T) {
	original := map[string]any{"data": map[string]any{"present": nil, "other": "keep"}}
	patch := map[string]any{"data": map[string]any{"present": "new", "missing": "new"}}
	fields := captureFields(original, patch)
	if len(fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(fields))
	}
	if fields[0].Path != "/data/missing" || fields[0].Existed {
		t.Fatalf("missing field recorded incorrectly: %#v", fields[0])
	}
	if fields[1].Path != "/data/present" || !fields[1].Existed || fields[1].ValueJSON != "null" {
		t.Fatalf("null field recorded incorrectly: %#v", fields[1])
	}
}

func TestRequestSurvivesRestartAndRestores(t *testing.T) {
	ctx := context.Background()
	request := requestObject("test", "default")
	request.SetUID(types.UID("request-uid"))
	request.SetGeneration(1)
	request.Object["spec"] = map[string]any{"mutations": []any{map[string]any{
		"target": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "target"},
		"patch":  map[string]any{"type": "merge", "value": map[string]any{"data": map[string]any{"mode": "paused"}}},
	}}}
	target := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "target", "namespace": "default", "uid": "target-uid"},
		"data":     map[string]any{"mode": "normal", "unrelated": "keep"},
	}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequest"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequestList"}, &unstructured.UnstructuredList{})
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, target).WithStatusSubresource(request).Build()
	r := &Reconciler{Client: c, Mapper: mapper}
	key := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test"}}
	ready := false
	for i := 0; i < 8; i++ {
		if _, err := r.Reconcile(ctx, key); err != nil {
			t.Fatal(err)
		}
		stored := requestObject("test", "default")
		if err := c.Get(ctx, key.NamespacedName, stored); err != nil {
			t.Fatal(err)
		}
		status, err := readStatus(stored)
		if err != nil {
			t.Fatal(err)
		}
		for _, condition := range status.Conditions {
			if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			break
		}
	}
	if !ready {
		t.Fatal("request did not become Ready")
	}
	storedTarget := &unstructured.Unstructured{}
	storedTarget.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target"}, storedTarget); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := unstructured.NestedString(storedTarget.Object, "data", "mode"); value != "paused" {
		t.Fatalf("mutation not applied: %q", value)
	}
	if err := unstructured.SetNestedField(storedTarget.Object, "changed while active", "data", "unrelated"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, storedTarget); err != nil {
		t.Fatal(err)
	}
	// A new reconciler instance represents a controller restart; only API state survives.
	r = &Reconciler{Client: c, Mapper: mapper}
	stored := requestObject("test", "default")
	if err := c.Get(ctx, key.NamespacedName, stored); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target"}, storedTarget); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := unstructured.NestedString(storedTarget.Object, "data", "mode"); value != "normal" {
		t.Fatalf("original not restored: %q", value)
	}
	if value, _, _ := unstructured.NestedString(storedTarget.Object, "data", "unrelated"); value != "changed while active" {
		t.Fatalf("unrelated field overwritten: %q", value)
	}
}

func TestRestoreConflictPreservesExternalValue(t *testing.T) {
	ctx := context.Background()
	request := requestObject("conflict", "default")
	request.SetUID(types.UID("request-uid"))
	request.Object["spec"] = map[string]any{"mutations": []any{map[string]any{
		"target": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "target"},
		"patch":  map[string]any{"type": "merge", "value": map[string]any{"data": map[string]any{"mode": "paused"}}},
	}}}
	target := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "target", "namespace": "default", "uid": "target-uid"},
		"data":     map[string]any{"mode": "normal"},
	}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequest"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequestList"}, &unstructured.UnstructuredList{})
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, target).WithStatusSubresource(request).Build()
	r := &Reconciler{Client: c, Mapper: mapper}
	key := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "conflict"}}
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	storedTarget := &unstructured.Unstructured{}
	storedTarget.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target"}, storedTarget); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(storedTarget.Object, "external", "data", "mode"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, storedTarget); err != nil {
		t.Fatal(err)
	}
	stored := requestObject("conflict", "default")
	if err := c.Get(ctx, key.NamespacedName, stored); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key.NamespacedName, stored); err != nil {
		t.Fatal(err)
	}
	if !contains(stored.GetFinalizers(), Finalizer) {
		t.Fatal("conflict removed finalizer")
	}
	status, err := readStatus(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Conditions) == 0 || status.Conditions[0].Reason != "RestoreConflict" {
		t.Fatalf("missing conflict condition: %#v", status.Conditions)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target"}, storedTarget); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := unstructured.NestedString(storedTarget.Object, "data", "mode"); value != "external" {
		t.Fatalf("external value overwritten: %q", value)
	}
}
