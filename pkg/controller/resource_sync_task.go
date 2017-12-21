package controller

import (
	"log"

	"github.com/atlassian/smith"
	smith_v1 "github.com/atlassian/smith/pkg/apis/smith/v1"
	"github.com/atlassian/smith/pkg/plugin"
	"github.com/atlassian/smith/pkg/util"

	sc_v1b1 "github.com/kubernetes-incubator/service-catalog/pkg/apis/servicecatalog/v1beta1"
	"github.com/pkg/errors"
	core_v1 "k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	unstructured_conversion "k8s.io/apimachinery/pkg/conversion/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// resourceStatus is one of "resourceStatus*" structs.
// It is a mechanism to communicate the status of a resource.
type resourceStatus interface{}

// resourceStatusDependenciesNotReady means resource processing is blocked by dependencies that are not ready.
type resourceStatusDependenciesNotReady struct {
	dependencies []smith_v1.ResourceName
}

// resourceStatusBlockedByError means there was an error, shouldn't do any work.
type resourceStatusBlockedByError struct {
}

// resourceStatusInProgress means resource is being processed by its controller.
type resourceStatusInProgress struct {
}

// resourceStatusReady means resource is ready.
type resourceStatusReady struct {
}

// resourceStatusError means there was an error processing this resource.
type resourceStatusError struct {
	err              error
	isRetriableError bool
}

type resourceInfo struct {
	actual *unstructured.Unstructured
	status resourceStatus
}

func (ri *resourceInfo) isReady() bool {
	_, ok := ri.status.(resourceStatusReady)
	return ok
}

func (ri *resourceInfo) fetchError() (bool, error) {
	if rse, ok := ri.status.(resourceStatusError); ok {
		return rse.isRetriableError, rse.err
	}
	return false, nil
}

type resourceSyncTask struct {
	smartClient        smith.SmartClient
	rc                 ReadyChecker
	store              Store
	specCheck          SpecCheck
	bundle             *smith_v1.Bundle
	processedResources map[smith_v1.ResourceName]*resourceInfo
	plugins            map[smith_v1.PluginName]plugin.Plugin
	scheme             *runtime.Scheme
	blockedOnError     bool
}

func (st *resourceSyncTask) processResource(res *smith_v1.Resource) resourceInfo {
	log.Printf("[WORKER][%s/%s] Processing resource %q", st.bundle.Namespace, st.bundle.Name, res.Name)

	// Check if all resource dependencies are ready (so we can start processing this one)
	status := st.checkAllDependenciesAreReady(res)
	if status != nil {
		return resourceInfo{
			status: status,
		}
	}

	// Eval spec
	spec, err := st.evalSpec(res)
	if err != nil {
		return resourceInfo{
			status: resourceStatusError{
				err: err,
			},
		}
	}

	// There was an error, shouldn't do any work
	if st.blockedOnError {
		return resourceInfo{
			status: resourceStatusBlockedByError{},
		}
	}

	// Create or update resource
	resUpdated, retriable, err := st.createOrUpdate(spec)
	if err != nil {
		return resourceInfo{
			actual: resUpdated,
			status: resourceStatusError{
				err:              err,
				isRetriableError: retriable,
			},
		}
	}

	// Check if resource is ready
	ready, retriable, err := st.rc.IsReady(resUpdated)
	if err != nil {
		return resourceInfo{
			actual: resUpdated,
			status: resourceStatusError{
				err:              errors.Wrap(err, "readiness check failed"),
				isRetriableError: retriable,
			},
		}
	}
	if ready {
		return resourceInfo{
			actual: resUpdated,
			status: resourceStatusReady{},
		}
	} else {
		return resourceInfo{
			actual: resUpdated,
			status: resourceStatusInProgress{},
		}
	}
}

func (st *resourceSyncTask) checkAllDependenciesAreReady(res *smith_v1.Resource) resourceStatus {
	var notReadyDependencies []smith_v1.ResourceName
	for _, dependency := range res.DependsOn {
		if !st.processedResources[dependency].isReady() {
			log.Printf("[WORKER][%s/%s] Dependency %q is required by resource %q but it's not ready", st.bundle.Namespace, st.bundle.Name, dependency, res.Name)
			notReadyDependencies = append(notReadyDependencies, dependency)
		}
	}
	if len(notReadyDependencies) > 0 {
		return resourceStatusDependenciesNotReady{
			dependencies: notReadyDependencies,
		}
	}
	return nil
}

// evalSpec evaluates the resource specification and returns the result.
func (st *resourceSyncTask) evalSpec(res *smith_v1.Resource) (*unstructured.Unstructured, error) {
	// 1. Process the spec
	var obj *unstructured.Unstructured
	var err error
	if res.Spec.Object != nil {
		obj, err = st.evalObjectSpec(res)
	} else if res.Spec.Plugin != nil {
		obj, err = st.evalPluginSpec(res)
	} else {
		return nil, errors.New("invalid resource")
	}

	if err != nil {
		return nil, err
	}

	// 2. Update label to point at the parent bundle
	obj.SetLabels(mergeLabels(
		st.bundle.Labels,
		obj.GetLabels(),
		map[string]string{smith.BundleNameLabel: st.bundle.Name}))

	// 3. Update OwnerReferences
	trueRef := true
	refs := obj.GetOwnerReferences()
	for i, ref := range refs {
		if ref.Controller != nil && *ref.Controller {
			return nil, errors.Errorf("cannot create resource with controller owner reference %v", ref)
		}
		refs[i].BlockOwnerDeletion = &trueRef
	}
	// Hardcode APIVersion/Kind because of https://github.com/kubernetes/client-go/issues/60
	refs = append(refs, meta_v1.OwnerReference{
		APIVersion:         smith_v1.BundleResourceGroupVersion,
		Kind:               smith_v1.BundleResourceKind,
		Name:               st.bundle.Name,
		UID:                st.bundle.UID,
		Controller:         &trueRef,
		BlockOwnerDeletion: &trueRef,
	})
	for _, dep := range res.DependsOn {
		obj := st.processedResources[dep].actual // this is ok because we've checked earlier that resources contains all dependencies
		refs = append(refs, meta_v1.OwnerReference{
			APIVersion:         obj.GetAPIVersion(),
			Kind:               obj.GetKind(),
			Name:               obj.GetName(),
			UID:                obj.GetUID(),
			BlockOwnerDeletion: &trueRef,
		})
	}
	obj.SetOwnerReferences(refs)

	return obj, nil
}

// evalPluginSpec evaluates the plugin resource specification and returns the result.
func (st *resourceSyncTask) evalPluginSpec(res *smith_v1.Resource) (*unstructured.Unstructured, error) {
	pluginInstance, ok := st.plugins[res.Spec.Plugin.Name]
	if !ok {
		return nil, errors.Errorf("no such plugin %q", res.Spec.Plugin.Name)
	}
	dependencies, err := st.prepareDependencies(res.DependsOn)
	if err != nil {
		return nil, err
	}

	result, err := pluginInstance.Process(res.Spec.Plugin.Spec, &plugin.Context{
		Dependencies: dependencies,
	})
	if err != nil {
		return nil, err
	}

	// Make sure plugin is returning us something that obeys the PluginSpec.
	object, err := util.RuntimeToUnstructured(result.Object)
	if err != nil {
		return nil, errors.Wrap(err, "plugin output cannot be converted from runtime.Object")
	}
	expectedGVK := pluginInstance.Describe().GVK
	if object.GroupVersionKind() != expectedGVK {
		return nil, errors.Errorf("unexpected GVK from plugin (wanted %s, got %s)", expectedGVK, object.GroupVersionKind())
	}
	// We are in charge of naming.
	object.SetName(res.Spec.Plugin.ObjectName)

	return object, nil
}

func (st *resourceSyncTask) prepareDependencies(dependsOn []smith_v1.ResourceName) (map[smith_v1.ResourceName]plugin.Dependency, error) {
	dependencies := make(map[smith_v1.ResourceName]plugin.Dependency, len(dependsOn))
	for _, name := range dependsOn {
		var dependency plugin.Dependency
		unstructuredActual := st.processedResources[name].actual
		gvk := unstructuredActual.GroupVersionKind()
		actual, err := st.scheme.New(gvk)
		if err != nil {
			return nil, err
		}
		if err = unstructured_conversion.DefaultConverter.FromUnstructured(unstructuredActual.Object, actual); err != nil {
			return nil, err
		}
		dependency.Actual = actual
		switch obj := actual.(type) {
		case *sc_v1b1.ServiceBinding:
			if err = st.prepareServiceBindingDependency(&dependency, obj); err != nil {
				return nil, errors.Wrapf(err, "error processing ServiceBinding %q", obj.Name)
			}
		}
		dependencies[name] = dependency
	}
	return dependencies, nil
}

func (st *resourceSyncTask) prepareServiceBindingDependency(dependency *plugin.Dependency, obj *sc_v1b1.ServiceBinding) error {
	secret, exists, err := st.store.Get(core_v1.SchemeGroupVersion.WithKind("Secret"), obj.Namespace, obj.Spec.SecretName)
	if err != nil {
		return errors.Wrap(err, "error finding output Secret")
	}
	if !exists {
		return errors.New("cannot find output Secret")
	}
	dependency.Outputs = append(dependency.Outputs, secret)

	serviceInstance, exists, err := st.store.Get(sc_v1b1.SchemeGroupVersion.WithKind("ServiceInstance"), obj.Namespace, obj.Spec.ServiceInstanceRef.Name)
	if err != nil {
		return errors.Wrapf(err, "error finding ServiceInstance %q", obj.Spec.ServiceInstanceRef.Name)
	}
	if !exists {
		return errors.Errorf("cannot find ServiceInstance %q", obj.Spec.ServiceInstanceRef.Name)
	}
	dependency.Auxiliary = append(dependency.Auxiliary, serviceInstance)
	return nil
}

// evalObjectSpec evaluates the regular resource specification and returns the result.
func (st *resourceSyncTask) evalObjectSpec(res *smith_v1.Resource) (*unstructured.Unstructured, error) {
	// Convert Spec to Unstructured
	spec, err := util.RuntimeToUnstructured(res.Spec.Object)

	if err != nil {
		return nil, err
	}

	// Process references
	sp := NewSpec(res.Name, st.processedResources, res.DependsOn)
	if err := sp.ProcessObject(spec.Object); err != nil {
		return nil, err
	}

	return spec, nil
}

// createOrUpdate creates or updates a resources.
func (st *resourceSyncTask) createOrUpdate(spec *unstructured.Unstructured) (actualRet *unstructured.Unstructured, retriableRet bool, e error) {
	// Prepare client
	gvk := spec.GroupVersionKind()
	resClient, err := st.smartClient.ForGVK(gvk, st.bundle.Namespace)
	if err != nil {
		return nil, false, errors.Wrapf(err, "failed to get the client for %q", gvk)
	}

	// Try to get the resource. We do read first to avoid generating unnecessary events.
	obj, exists, err := st.store.Get(gvk, st.bundle.Namespace, spec.GetName())
	if err != nil {
		// Unexpected error
		return nil, false, errors.Wrap(err, "failed to get object from the Store")
	}
	if exists {
		log.Printf("[WORKER][%s/%s] Object %s %q found, checking spec", st.bundle.Namespace, st.bundle.Name, gvk, spec.GetName())
		return st.updateResource(resClient, spec, obj)
	}
	log.Printf("[WORKER][%s/%s] Object %s %q not found, creating", st.bundle.Namespace, st.bundle.Name, gvk, spec.GetName())
	return st.createResource(resClient, spec)
}

func (st *resourceSyncTask) createResource(resClient dynamic.ResourceInterface, spec *unstructured.Unstructured) (actualRet *unstructured.Unstructured, retriableError bool, e error) {
	gvk := spec.GroupVersionKind()
	response, err := resClient.Create(spec)
	if err == nil {
		log.Printf("[WORKER][%s/%s] Object %s %q created", st.bundle.Namespace, st.bundle.Name, gvk, spec.GetName())
		return response, false, nil
	}
	if api_errors.IsAlreadyExists(err) {
		// We let the next processKey() iteration, triggered by someone else creating the resource, to finish the work.
		err = api_errors.NewConflict(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, spec.GetName(), err)
		return nil, false, errors.Wrap(err, "object found, but not in Store yet (will re-process)")
	}
	// Unexpected error, will retry
	return nil, true, err
}

// Mutates spec and actual.
func (st *resourceSyncTask) updateResource(resClient dynamic.ResourceInterface, spec *unstructured.Unstructured, actual runtime.Object) (actualRet *unstructured.Unstructured, retriableError bool, e error) {
	actualMeta := actual.(meta_v1.Object)
	// Check that the object is not marked for deletion
	if actualMeta.GetDeletionTimestamp() != nil {
		return nil, false, errors.New("object is marked for deletion")
	}

	// Check that this bundle owns the object
	if !meta_v1.IsControlledBy(actualMeta, st.bundle) {
		return nil, false, errors.New("object is not owned by the Bundle")
	}

	// Compare spec and existing resource
	updated, match, err := st.specCheck.CompareActualVsSpec(spec, actual)
	if err != nil {
		return nil, false, errors.Wrap(err, "specification check failed")
	}
	if match {
		log.Printf("[WORKER][%s/%s] Object %q has correct spec", st.bundle.Namespace, st.bundle.Name, spec.GetName())
		return updated, false, nil
	}

	// Update if different
	updated, err = resClient.Update(updated)
	if err != nil {
		if api_errors.IsConflict(err) {
			// We let the next processKey() iteration, triggered by someone else updating the resource, finish the work.
			return nil, false, errors.Wrap(err, "object update resulted in conflict (will re-process)")
		}
		// Unexpected error, will retry
		return nil, true, err
	}
	log.Printf("[WORKER][%s/%s] Object %q updated", st.bundle.Namespace, st.bundle.Name, spec.GetName())
	return updated, false, nil
}

func mergeLabels(labels ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range labels {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}