//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rejinka/reprise/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestAPIRequestLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run API integration test")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd"}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	}()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(mgr.GetScheme()); err != nil {
		t.Fatal(err)
	}
	gvk := schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequest"}
	prototype := &unstructured.Unstructured{}
	prototype.SetGroupVersionKind(gvk)
	if err := ctrl.NewControllerManagedBy(mgr).For(prototype).Complete(&controller.Reconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Mapper: mgr.GetRESTMapper()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	api := mgr.GetAPIReader()
	writer := mgr.GetClient()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "reprise-test"}}
	if err := writer.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	target := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns.Name}, Data: map[string]string{"mode": "normal"}}
	if err := writer.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	request := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind,
		"metadata": map[string]any{"name": "test", "namespace": ns.Name},
		"spec": map[string]any{"mutations": []any{map[string]any{
			"target": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "target"},
			"patch":  map[string]any{"type": "merge", "value": map[string]any{"data": map[string]any{"mode": "paused"}}},
		}}},
	}}
	if err := writer.Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: ns.Name, Name: request.GetName()}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(gvk)
		if err := api.Get(ctx, key, current); err == nil {
			conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
			for _, raw := range conditions {
				condition := raw.(map[string]any)
				if condition["type"] == "Ready" && condition["status"] == "True" {
					goto ready
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("request never became Ready")
ready:
	if err := api.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
		t.Fatal(err)
	}
	if target.Data["mode"] != "paused" {
		t.Fatalf("target mode = %q", target.Data["mode"])
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)
	if err := api.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	mutations, _, _ := unstructured.NestedSlice(current.Object, "spec", "mutations")
	mutation := mutations[0].(map[string]any)
	patch := mutation["patch"].(map[string]any)
	patch["value"] = map[string]any{"data": map[string]any{"mode": "changed"}}
	mutations[0] = mutation
	if err := unstructured.SetNestedSlice(current.Object, mutations, "spec", "mutations"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Update(ctx, current); err == nil {
		t.Fatal("CRD accepted an immutable spec update")
	}
	if err := api.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if err := writer.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline.Add(30 * time.Second)) {
		err := api.Get(ctx, key, current)
		if errors.IsNotFound(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := api.Get(ctx, key, current); !errors.IsNotFound(err) {
		t.Fatalf("request remains after delete: %v", err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
		t.Fatal(err)
	}
	if target.Data["mode"] != "normal" {
		t.Fatalf("target not restored: %q", target.Data["mode"])
	}
	clusterRequest := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind,
		"metadata": map[string]any{"name": "cluster-target", "namespace": ns.Name},
		"spec": map[string]any{"mutations": []any{map[string]any{
			"target": map[string]any{"apiVersion": "v1", "kind": "Namespace", "name": ns.Name},
			"patch":  map[string]any{"type": "merge", "value": map[string]any{"metadata": map[string]any{"labels": map[string]any{"reprise.naji-dev.de/test": "active"}}}},
		}}},
	}}
	if err := writer.Create(ctx, clusterRequest); err != nil {
		t.Fatal(err)
	}
	clusterKey := client.ObjectKey{Namespace: ns.Name, Name: clusterRequest.GetName()}
	clusterReady := false
	for time.Now().Before(deadline.Add(30 * time.Second)) {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(gvk)
		if err := api.Get(ctx, clusterKey, current); err == nil {
			conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
			for _, raw := range conditions {
				condition := raw.(map[string]any)
				if condition["type"] == "Ready" && condition["status"] == "True" {
					clusterReady = true
				}
			}
		}
		if clusterReady {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !clusterReady {
		t.Fatal("cluster-scoped target did not become Ready")
	}
	if err := api.Get(ctx, client.ObjectKey{Name: ns.Name}, ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels["reprise.naji-dev.de/test"] != "active" {
		t.Fatalf("cluster-scoped patch not applied: %#v", ns.Labels)
	}
	current = &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)
	if err := api.Get(ctx, clusterKey, current); err != nil {
		t.Fatal(err)
	}
	if err := writer.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline.Add(60 * time.Second)) {
		err := api.Get(ctx, clusterKey, current)
		if errors.IsNotFound(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := api.Get(ctx, clusterKey, current); !errors.IsNotFound(err) {
		t.Fatalf("cluster request remains after delete: %v", err)
	}
	if err := api.Get(ctx, client.ObjectKey{Name: ns.Name}, ns); err != nil {
		t.Fatal(err)
	}
	if _, present := ns.Labels["reprise.naji-dev.de/test"]; present {
		t.Fatalf("cluster-scoped label not restored: %#v", ns.Labels)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop")
	}
}
