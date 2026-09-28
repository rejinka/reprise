package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	api "github.com/rejinka/reprise/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	Finalizer         = "reprise.naji-dev.de/restore"
	LockAnnotation    = "reprise.naji-dev.de/lock"
	AppliedAnnotation = "reprise.naji-dev.de/applied"
)

func MetricsOptions(addr string) metricsserver.Options {
	return metricsserver.Options{BindAddress: addr}
}

type Target = api.Target
type Mutation = api.Mutation
type Spec = api.RepriseRequestSpec
type Field = api.RecoveryField
type Entry = api.RecoveryEntry
type Status = api.RepriseRequestStatus
type Reconciler struct {
	Client client.Client
	Reader client.Reader
	Mapper meta.RESTMapper
}

func (r *Reconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := requestObject(req.Name, req.Namespace)
	if err := r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status, err := readStatus(obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return r.restore(ctx, obj, status)
	}
	if !contains(obj.GetFinalizers(), Finalizer) {
		base := obj.DeepCopy()
		obj.SetFinalizers(append(obj.GetFinalizers(), Finalizer))
		if err := r.Client.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	spec, err := readSpec(obj)
	if err != nil {
		return r.fail(ctx, obj, status, "InvalidSpec", err)
	}
	canonical, _ := json.Marshal(spec)
	if status.SpecJSON != "" && status.SpecJSON != string(canonical) {
		return r.fail(ctx, obj, status, "SpecChanged", errors.New("spec changed after recovery state was captured"))
	}
	if len(status.Journal) == 0 {
		journal, err := r.capture(ctx, obj, spec)
		if err != nil {
			return r.fail(ctx, obj, status, "CaptureFailed", err)
		}
		status.Journal, status.SpecJSON, status.Phase = journal, string(canonical), "Captured"
		if err := writeStatus(ctx, r.Client, obj, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.lockAll(ctx, obj, status.Journal); err != nil {
		return r.fail(ctx, obj, status, "TargetLocked", err)
	}
	for i, entry := range status.Journal {
		if err := r.apply(ctx, obj, entry, spec.Mutations[i]); err != nil {
			return r.fail(ctx, obj, status, "ApplyFailed", err)
		}
	}
	for _, entry := range status.Journal {
		if err := r.verify(ctx, obj, entry); err != nil {
			return r.fail(ctx, obj, status, "VerificationPending", err)
		}
	}
	status.ObservedGeneration = obj.GetGeneration()
	status.Phase = "Applied"
	setReady(&status, obj, metav1.ConditionTrue, "MutationsApplied", "All mutations are applied and verified")
	if err := writeStatus(ctx, r.Client, obj, status); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *Reconciler) fail(ctx context.Context, obj *unstructured.Unstructured, status Status, reason string, cause error) (ctrl.Result, error) {
	status.Phase = "Pending"
	status.ObservedGeneration = obj.GetGeneration()
	setReady(&status, obj, metav1.ConditionFalse, reason, cause.Error())
	if err := writeStatus(ctx, r.Client, obj, status); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *Reconciler) capture(ctx context.Context, req *unstructured.Unstructured, spec Spec) ([]Entry, error) {
	if len(spec.Mutations) == 0 {
		return nil, errors.New("at least one mutation required")
	}
	seen := map[string]bool{}
	entries := make([]Entry, 0, len(spec.Mutations))
	for _, mutation := range spec.Mutations {
		t := mutation.Target
		explicitNamespace := t.Namespace != ""
		if t.Namespace == "" {
			t.Namespace = req.GetNamespace()
		}
		if t.APIVersion == "" || t.Kind == "" || t.Name == "" {
			return nil, errors.New("target apiVersion, kind and name are required")
		}
		if t.Kind == "Secret" && t.APIVersion == "v1" {
			return nil, errors.New("Secret targets are forbidden")
		}
		if mutation.Patch.Type != "merge" || mutation.Patch.Value == nil {
			return nil, errors.New("only nonempty merge patches are supported")
		}
		if _, ok := mutation.Patch.Value["status"]; ok {
			return nil, errors.New("status patches are not supported")
		}
		if err := checkReserved(mutation.Patch.Value); err != nil {
			return nil, err
		}
		obj, normalized, err := r.getTarget(ctx, t)
		if err != nil {
			return nil, err
		}
		if explicitNamespace && normalized.Namespace == "" {
			return nil, fmt.Errorf("cluster-scoped target %s must not set namespace", t.Name)
		}
		key := targetKey(normalized)
		if seen[key] {
			return nil, fmt.Errorf("duplicate target %s", key)
		}
		seen[key] = true
		if owner := obj.GetAnnotations()[LockAnnotation]; owner != "" && owner != string(req.GetUID()) {
			return nil, fmt.Errorf("%s locked by %s", key, owner)
		}
		fields := captureFields(obj.Object, mutation.Patch.Value)
		if len(fields) == 0 {
			return nil, fmt.Errorf("empty patch for %s", key)
		}
		entry := Entry{Target: normalized, UID: obj.GetUID(), Fields: fields}
		if scaleToZero(normalized, mutation.Patch.Value) {
			if normalized.Kind == "ReplicaSet" {
				for _, owner := range obj.GetOwnerReferences() {
					if owner.Controller != nil && *owner.Controller {
						return nil, fmt.Errorf("controlled ReplicaSet %s must be scaled through its owner", key)
					}
				}
			}
			raw, found, _ := unstructured.NestedMap(obj.Object, "spec", "selector")
			if !found {
				return nil, fmt.Errorf("%s has no selector", key)
			}
			b, _ := json.Marshal(raw)
			var selector metav1.LabelSelector
			if err := json.Unmarshal(b, &selector); err != nil {
				return nil, err
			}
			if _, err := metav1.LabelSelectorAsSelector(&selector); err != nil {
				return nil, err
			}
			entry.ScaleZero = true
			entry.Selector = &selector
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (r *Reconciler) lockAll(ctx context.Context, req *unstructured.Unstructured, entries []Entry) error {
	ordered := append([]Entry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return targetKey(ordered[i].Target) < targetKey(ordered[j].Target) })
	for _, entry := range ordered {
		obj, _, err := r.getTarget(ctx, entry.Target)
		if err != nil {
			return err
		}
		if obj.GetUID() != entry.UID {
			return fmt.Errorf("%s was replaced", targetKey(entry.Target))
		}
		owner := obj.GetAnnotations()[LockAnnotation]
		if owner == string(req.GetUID()) {
			continue
		}
		if owner != "" {
			return fmt.Errorf("%s locked by another request", targetKey(entry.Target))
		}
		ops := []patchOp{{Op: "test", Path: "/metadata/resourceVersion", Value: obj.GetResourceVersion()}}
		ops = append(ops, annotationOp(obj, LockAnnotation, string(req.GetUID()))...)
		if err := r.patch(ctx, obj, ops); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) apply(ctx context.Context, req *unstructured.Unstructured, entry Entry, mutation Mutation) error {
	obj, _, err := r.getTarget(ctx, entry.Target)
	if err != nil {
		return err
	}
	if obj.GetUID() != entry.UID {
		return errors.New("target UID changed")
	}
	if obj.GetAnnotations()[LockAnnotation] != string(req.GetUID()) {
		return errors.New("target lock lost")
	}
	if obj.GetAnnotations()[AppliedAnnotation] == string(req.GetUID()) {
		return nil
	}
	for _, f := range entry.Fields {
		exists, v := lookup(obj.Object, f.Path)
		if exists != f.Existed || (exists && jsonText(v) != f.ValueJSON) {
			return fmt.Errorf("target changed before apply at %s", f.Path)
		}
	}
	ops := []patchOp{{Op: "test", Path: "/metadata/resourceVersion", Value: obj.GetResourceVersion()}}
	for _, f := range entry.Fields {
		ops = append(ops, fieldOp(f.Path, f.ExpectedExists, f.ExpectedJSON, f.Existed))
	}
	ops = append(ops, annotationOp(obj, AppliedAnnotation, string(req.GetUID()))...)
	return r.patch(ctx, obj, ops)
}

func (r *Reconciler) verify(ctx context.Context, req *unstructured.Unstructured, entry Entry) error {
	obj, _, err := r.getTarget(ctx, entry.Target)
	if err != nil {
		return err
	}
	if obj.GetUID() != entry.UID {
		return errors.New("target UID changed")
	}
	if obj.GetAnnotations()[LockAnnotation] != string(req.GetUID()) || obj.GetAnnotations()[AppliedAnnotation] != string(req.GetUID()) {
		return errors.New("target lock or applied marker missing")
	}
	for _, f := range entry.Fields {
		exists, v := lookup(obj.Object, f.Path)
		if exists != f.ExpectedExists || (exists && jsonText(v) != f.ExpectedJSON) {
			return fmt.Errorf("target drifted at %s", f.Path)
		}
	}
	if entry.ScaleZero {
		generation, _, _ := unstructured.NestedInt64(obj.Object, "metadata", "generation")
		observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
		if observed < generation {
			return errors.New("workload controller has not observed the scaled generation")
		}
		selector, err := metav1.LabelSelectorAsSelector(entry.Selector)
		if err != nil {
			return err
		}
		pods := &unstructured.UnstructuredList{}
		pods.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "PodList"})
		if err := r.reader().List(ctx, pods, client.InNamespace(entry.Target.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return err
		}
		if len(pods.Items) > 0 {
			return fmt.Errorf("%d matching pods remain", len(pods.Items))
		}
	}
	return nil
}

func (r *Reconciler) restore(ctx context.Context, req *unstructured.Unstructured, status Status) (ctrl.Result, error) {
	var conflicts []string
	for i := len(status.Journal) - 1; i >= 0; i-- {
		entry := status.Journal[i]
		if err := r.restoreEntry(ctx, req, entry); err != nil {
			conflicts = append(conflicts, fmt.Sprintf("%s: %v", targetKey(entry.Target), err))
		}
	}
	if len(conflicts) != 0 {
		return r.fail(ctx, req, status, "RestoreConflict", errors.New(strings.Join(conflicts, "; ")))
	}
	base := req.DeepCopy()
	req.SetFinalizers(remove(req.GetFinalizers(), Finalizer))
	if err := r.Client.Patch(ctx, req, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) restoreEntry(ctx context.Context, req *unstructured.Unstructured, entry Entry) error {
	obj, _, err := r.getTarget(ctx, entry.Target)
	if err != nil {
		return err
	}
	if obj.GetUID() != entry.UID {
		return errors.New("target was replaced; refusing to patch replacement")
	}
	if owner := obj.GetAnnotations()[LockAnnotation]; owner != "" && owner != string(req.GetUID()) {
		return errors.New("target lock belongs to another request")
	}
	if obj.GetAnnotations()[AppliedAnnotation] == string(req.GetUID()) {
		for _, f := range entry.Fields {
			exists, v := lookup(obj.Object, f.Path)
			if exists != f.ExpectedExists || (exists && jsonText(v) != f.ExpectedJSON) {
				return fmt.Errorf("field %s changed while request active", f.Path)
			}
		}
		ops := []patchOp{{Op: "test", Path: "/metadata/resourceVersion", Value: obj.GetResourceVersion()}}
		for j := len(entry.Fields) - 1; j >= 0; j-- {
			f := entry.Fields[j]
			ops = append(ops, fieldOp(f.Path, f.Existed, f.ValueJSON, f.ExpectedExists))
		}
		ops = append(ops, patchOp{Op: "remove", Path: "/metadata/annotations/" + escape(AppliedAnnotation)})
		if err := r.patch(ctx, obj, ops); err != nil {
			return err
		}
		obj, _, err = r.getTarget(ctx, entry.Target)
		if err != nil {
			return err
		}
	}
	for _, f := range entry.Fields {
		exists, v := lookup(obj.Object, f.Path)
		if exists != f.Existed || (exists && jsonText(v) != f.ValueJSON) {
			return fmt.Errorf("field %s not restored", f.Path)
		}
	}
	if obj.GetAnnotations()[LockAnnotation] == string(req.GetUID()) {
		ops := []patchOp{{Op: "test", Path: "/metadata/resourceVersion", Value: obj.GetResourceVersion()}, {Op: "remove", Path: "/metadata/annotations/" + escape(LockAnnotation)}}
		if err := r.patch(ctx, obj, ops); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) getTarget(ctx context.Context, t Target) (*unstructured.Unstructured, Target, error) {
	gv, err := schema.ParseGroupVersion(t.APIVersion)
	if err != nil {
		return nil, t, err
	}
	gvk := gv.WithKind(t.Kind)
	mapping, err := r.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, t, err
	}
	if mapping.Scope.Name() == meta.RESTScopeNameRoot {
		t.Namespace = ""
	} else if t.Namespace == "" {
		return nil, t, errors.New("namespace required for namespaced target")
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, obj); err != nil {
		return nil, t, err
	}
	return obj, t, nil
}
func (r *Reconciler) patch(ctx context.Context, obj *unstructured.Unstructured, ops []patchOp) error {
	b, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	return r.Client.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, b))
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func fieldOp(path string, exists bool, value string, previously bool) patchOp {
	if !exists {
		return patchOp{Op: "remove", Path: path}
	}
	if previously {
		return patchOp{Op: "replace", Path: path, Value: json.RawMessage(value)}
	}
	return patchOp{Op: "add", Path: path, Value: json.RawMessage(value)}
}

// JSON text preserves int64 values across the unstructured status round-trip.
func jsonText(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
func annotationOp(obj *unstructured.Unstructured, key, value string) []patchOp {
	if obj.GetAnnotations() == nil {
		return []patchOp{{Op: "add", Path: "/metadata/annotations", Value: map[string]string{key: value}}}
	}
	return []patchOp{{Op: "add", Path: "/metadata/annotations/" + escape(key), Value: value}}
}
func captureFields(original, patch map[string]any) []Field {
	var fields []Field
	var visit func(map[string]any, map[string]any, string)
	visit = func(o, p map[string]any, prefix string) {
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			path := prefix + "/" + escape(k)
			old, exists := o[k]
			next := p[k]
			if nested, ok := next.(map[string]any); ok {
				if prior, ok := old.(map[string]any); ok {
					visit(prior, nested, path)
					continue
				}
			}
			expectedExists := next != nil
			if expectedExists {
				next = mergeValue(nil, next)
			}
			fields = append(fields, Field{Path: path, Existed: exists, ValueJSON: jsonText(old), ExpectedExists: expectedExists, ExpectedJSON: jsonText(next)})
		}
	}
	visit(original, patch, "")
	return fields
}
func mergeValue(old, next any) any {
	m, ok := next.(map[string]any)
	if !ok {
		return next
	}
	out := map[string]any{}
	if prior, ok := old.(map[string]any); ok {
		for k, v := range prior {
			out[k] = v
		}
	}
	for k, v := range m {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = mergeValue(out[k], v)
		}
	}
	return out
}
func lookup(obj map[string]any, path string) (bool, any) {
	var cur any = obj
	for _, piece := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false, nil
		}
		key := strings.ReplaceAll(strings.ReplaceAll(piece, "~1", "/"), "~0", "~")
		var found bool
		cur, found = m[key]
		if !found {
			return false, nil
		}
	}
	return true, cur
}
func escape(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }
func scaleToZero(t Target, patch map[string]any) bool {
	if t.APIVersion != "apps/v1" || (t.Kind != "Deployment" && t.Kind != "StatefulSet" && t.Kind != "ReplicaSet") {
		return false
	}
	spec, ok := patch["spec"].(map[string]any)
	if !ok {
		return false
	}
	replicas, ok := spec["replicas"]
	if !ok {
		return false
	}
	switch n := replicas.(type) {
	case float64:
		return n == 0
	case int:
		return n == 0
	case int64:
		return n == 0
	case json.Number:
		v, _ := strconv.ParseInt(string(n), 10, 64)
		return v == 0
	}
	return false
}
func checkReserved(p map[string]any) error {
	for _, k := range []string{"apiVersion", "kind"} {
		if _, ok := p[k]; ok {
			return fmt.Errorf("%s is reserved", k)
		}
	}
	rawMetadata, present := p["metadata"]
	if !present {
		return nil
	}
	metadata, ok := rawMetadata.(map[string]any)
	if !ok {
		return errors.New("metadata patch must be an object")
	}
	for _, k := range []string{"name", "namespace", "uid", "resourceVersion", "generation", "ownerReferences", "finalizers", "deletionTimestamp"} {
		if _, ok := metadata[k]; ok {
			return fmt.Errorf("metadata.%s is reserved", k)
		}
	}
	if raw, ok := metadata["annotations"]; ok {
		if _, valid := raw.(map[string]any); !valid {
			return errors.New("metadata.annotations patch must be an object")
		}
	}
	if annotations, ok := metadata["annotations"].(map[string]any); ok {
		for k := range annotations {
			if strings.HasPrefix(k, "reprise.naji-dev.de/") {
				return fmt.Errorf("annotation %s is reserved", k)
			}
		}
	}
	return nil
}
func readSpec(obj *unstructured.Unstructured) (Spec, error) {
	var s Spec
	b, err := json.Marshal(obj.Object["spec"])
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
func readStatus(obj *unstructured.Unstructured) (Status, error) {
	var s Status
	raw := obj.Object["status"]
	if raw == nil {
		return s, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
func writeStatus(ctx context.Context, c client.Client, obj *unstructured.Unstructured, s Status) error {
	previous, err := readStatus(obj)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(previous, s) {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	obj.Object["status"] = raw
	return c.Status().Update(ctx, obj)
}
func setReady(s *Status, obj *unstructured.Unstructured, state metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: "Ready", Status: state, ObservedGeneration: obj.GetGeneration(), Reason: reason, Message: message})
}
func requestObject(name, namespace string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequest"})
	o.SetName(name)
	o.SetNamespace(namespace)
	return o
}
func targetKey(t Target) string {
	return t.APIVersion + "/" + t.Kind + "/" + t.Namespace + "/" + t.Name
}
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func remove(xs []string, x string) []string {
	out := make([]string, 0, len(xs))
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
