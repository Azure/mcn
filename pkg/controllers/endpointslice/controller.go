/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

// package endpointslice features the EndpointSlice controller for exporting an EndpointSlice from a member cluster
// to its fleet.
package endpointslice

import (
	"context"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	fleetnetv1alpha1 "go.goms.io/fleet-networking/api/v1alpha1"
)

const (
	endpointSliceUniqueNameLabel = "networking.fleet.azure.com/fleet-unique-name"
)

// skipOrUnexportEndpointSliceOp describes the op the controller should take on an EndpointSlice, specifically
// whether to skip reconciling an EndpointSlice, and whether to unexport an EndpointSlice.
type skipOrUnexportEndpointSliceOp int

const (
	// shouldSkipEndpointSliceOp notes that an EndpointSlice should be skipped for reconciliation.
	shouldSkipEndpointSliceOp skipOrUnexportEndpointSliceOp = 0
	// shouldUnexportEndpointSliceOp notes that an EndpointSlice should be unexported.
	shouldUnexportEndpointSliceOp skipOrUnexportEndpointSliceOp = 1
	// noSkipOrUnexportNeededOp notes that an EndpointSlice should not be skipped or unexported.
	noSkipOrUnexportNeededOp skipOrUnexportEndpointSliceOp = 2
)

// Reconciler reconciles the export of an EndpointSlice.
type Reconciler struct {
	memberClusterID string
	memberClient    client.Client
	hubClient       client.Client
	// The namespace reserved for the current member cluster in the hub cluster.
	hubNamespace string
}

//+kubebuilder:rbac:groups=networking.fleet.azure.com,resources=endpointsliceexports,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=endpointslices,verbs=get;list;watch

// Reconcile exports an EndpointSlice.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	endpointSliceRef := klog.KRef(req.Namespace, req.Name)
	startTime := time.Now()
	klog.V(2).InfoS("Reconciliation starts", "endpointSlice", endpointSliceRef)
	defer func() {
		latency := time.Since(startTime).Seconds()
		klog.V(2).InfoS("Reconciliation ends", "endpointSlice", endpointSliceRef, "latency", latency)
	}()

	// Retrieve the EndpointSlice object.
	var endpointSlice discoveryv1.EndpointSlice
	endpointSliceKey := types.NamespacedName{Namespace: req.Namespace, Name: req.Name}
	if err := r.memberClient.Get(ctx, endpointSliceKey, &endpointSlice); err != nil {
		// Skip the reconciliation if the EndpointSlice does not exist; this should only happen when an EndpointSlice
		// is deleted right before the controller gets a chance to reconcile it. If the EndpointSlice has never
		// been exported to the fleet, no action is required on this controller's end; on the other hand, if the
		// EndpointSlice has been exported before, this may result in an EndpointSlice being left over on the
		// hub cluster, and it is up to another controller, EndpointSliceExport controller, to pick up the leftover
		// and clean it out.
		klog.ErrorS(err, "Failed to get endpoint slice", "endpointSlice", endpointSliceRef)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if the EndpointSlice should be skipped for reconciliation or unexported.
	skipOrUnexportOp, err := r.shouldSkipOrUnexportEndpointSlice(ctx, &endpointSlice)
	switch skipOrUnexportOp {
	case shouldSkipEndpointSliceOp:
		// Skip reconciling the EndpointSlice.
		klog.V(4).InfoS("Endpoint slice should be skipped for reconciliation", "endpointSlice", endpointSliceRef)
		return ctrl.Result{}, nil
	case shouldUnexportEndpointSliceOp:
		// Unexport the EndpointSlice.
		klog.V(4).InfoS("Endpoint slice should be unexported", "endpointSlice", endpointSliceRef)
		if err := r.unexportEndpointSlice(ctx, &endpointSlice); err != nil {
			klog.ErrorS(err, "Failed to unexport the endpoint slice", "endpointSlice", endpointSliceRef)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	case noSkipOrUnexportNeededOp:
		if err != nil {
			// An unexpected error occurs.
			klog.ErrorS(err,
				"Failed to determine whether an endpoint slice should be skipped for reconciliation or unexported",
				"endpointSlice", endpointSliceRef)
			return ctrl.Result{}, err
		}
	}

	// Retrieve the unique name assigned (if any), or format a new one and assign it.
	fleetUniqueName, ok := endpointSlice.Labels[endpointSliceUniqueNameLabel]
	if !ok {
		var err error
		// Unique name label must be added before an EndpointSlice is exported.
		fleetUniqueName, err = r.assignUniqueNameAsLabel(ctx, &endpointSlice)
		if err != nil {
			klog.ErrorS(err, "Failed to assign unique name as a label", "endpointSlice", endpointSliceRef)
			return ctrl.Result{}, err
		}
	}

	// Create an EndpointSliceExport in the hub cluster if the EndpointSlice has never been exported; otherwise
	// update the corresponding EndpointSliceExport.
	extractedEndpoints := extractEndpointsFromEndpointSlice(&endpointSlice)
	endpointSliceExport := fleetnetv1alpha1.EndpointSliceExport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.hubNamespace,
			Name:      fleetUniqueName,
		},
	}
	createOrUpdateOp, err := controllerutil.CreateOrUpdate(ctx, r.hubClient, &endpointSliceExport, func() error {
		endpointSliceExport.Spec = fleetnetv1alpha1.EndpointSliceExportSpec{
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   extractedEndpoints,
			Ports:       endpointSlice.Ports,
			EndpointSliceReference: fleetnetv1alpha1.ExportedObjectReference{
				ClusterID:       r.memberClusterID,
				APIVersion:      endpointSlice.APIVersion,
				Kind:            endpointSlice.Kind,
				Namespace:       endpointSlice.Namespace,
				Name:            endpointSlice.Name,
				ResourceVersion: endpointSlice.ResourceVersion,
				Generation:      endpointSlice.Generation,
				UID:             endpointSlice.UID,
			},
		}

		return nil
	})
	if err != nil {
		klog.ErrorS(err,
			"Failed to create/update endpointslice export",
			"endpointSlice", endpointSliceRef,
			"endpointSliceExport", klog.KRef(r.hubNamespace, fleetUniqueName),
			"op", createOrUpdateOp)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	// Enqueue EndpointSlices for processing when a ServiceExport changes.
	eventHandlers := handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
		endpointSliceList := &discoveryv1.EndpointSliceList{}
		listOpts := client.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{
				discoveryv1.LabelServiceName: o.GetName(),
			}),
			Namespace: o.GetNamespace(),
		}
		if err := r.memberClient.List(ctx, endpointSliceList, &listOpts); err != nil {
			klog.ErrorS(err,
				"Failed to list endpoint slices in use by a service",
				"serviceExport", klog.KRef(o.GetNamespace(), o.GetName()),
			)
			return []reconcile.Request{}
		}
		reqs := []reconcile.Request{}
		for _, endpointSlice := range endpointSliceList.Items {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: endpointSlice.Namespace, Name: endpointSlice.Name},
			})
		}
		return reqs
	})

	// EndpointSlice controller watches over EndpointSlice and ServiceExport objects.
	return ctrl.NewControllerManagedBy(mgr).
		For(&discoveryv1.EndpointSlice{}).
		Watches(&source.Kind{Type: &fleetnetv1alpha1.ServiceExport{}}, eventHandlers).
		Complete(r)
}

// shouldSkipOrUnexportEndpointSlice returns the op the controller should take on an EndpointSlice, specifically
// whether to skip reconciling an EndpointSlice, and whether to unexport an EndpointSlice.
//
// The controller can only export an EndpointSlice if
// * the EndpointSlice is in use by a Service that has been successfully exported (valid with no conflicts); and
// * the EndpointSlice has not been deleted.
//
// If an EndpointSlice has been exported before, but
// * its owner Service has not been, or is no longer, exported; or
// * the EndpointSlice itself has been deleted
// the EndpointSlice should be unexported.
//
// EndpointSlices that are
// * not exportable; or
// * not owned by a successfully exported Service
// should never be reconciled with this controller.
func (r *Reconciler) shouldSkipOrUnexportEndpointSlice(ctx context.Context,
	endpointSlice *discoveryv1.EndpointSlice) (skipOrUnexportEndpointSliceOp, error) {
	// Skip the reconciliation if the EndpointSlice is not permanently exportable.
	if isEndpointSlicePermanentlyUnexportable(endpointSlice) {
		return shouldSkipEndpointSliceOp, nil
	}

	// If the Service name label is absent, the EndpointSlice is not in use by a Service and thus cannot
	// be exported.
	svcName, hasSvcNameLabel := endpointSlice.Labels[discoveryv1.LabelServiceName]
	// It is guaranteed that if there is no unique name assigned to an EndpointSlice as a label, no attempt has
	// been made to export an EndpointSlice.
	_, hasUniqueNameLabel := endpointSlice.Labels[endpointSliceUniqueNameLabel]

	if !hasSvcNameLabel {
		if !hasUniqueNameLabel {
			// The Service is not in use by a Service and does not have a unique name label (i.e. it has not been
			// exported before); it should be skipped for further processing.
			return shouldSkipEndpointSliceOp, nil
		}
		// The Service is not in use by a Service but has a unique name label (i.e. it might have been exported);
		// this could happen on an orphaned exported EndpointSlice, which should be unexported.
		return shouldUnexportEndpointSliceOp, nil
	}

	// Retrieve the Service Export.
	svcExport := &fleetnetv1alpha1.ServiceExport{}
	err := r.memberClient.Get(ctx, types.NamespacedName{Namespace: endpointSlice.Namespace, Name: svcName}, svcExport)
	switch {
	case errors.IsNotFound(err) && hasUniqueNameLabel:
		// The Service using the EndpointSlice is not exported but the EndpointSlice has a unique name label
		// present (i.e. it might have been exported); the EndpointSlice should be unexported.
		return shouldUnexportEndpointSliceOp, nil
	case errors.IsNotFound(err) && !hasUniqueNameLabel:
		// The Service using the EndpointSlice is not exported and the EndpointSlice has no unique name label
		// present (i.e. it has not been exported before); the EndpointSlice should be skipped for further processing.
		return shouldSkipEndpointSliceOp, nil
	case err != nil:
		// An unexpected error has occurred.
		return noSkipOrUnexportNeededOp, err
	}

	// Check if the ServiceExport is valid with no conflicts.
	validCond := meta.FindStatusCondition(svcExport.Status.Conditions, string(fleetnetv1alpha1.ServiceExportValid))
	conflictCond := meta.FindStatusCondition(svcExport.Status.Conditions, string(fleetnetv1alpha1.ServiceExportConflict))
	isValid := (validCond != nil && validCond.Status == metav1.ConditionTrue)
	hasNoConflict := (conflictCond != nil && conflictCond.Status == metav1.ConditionFalse)
	isValidWithNoConflict := (isValid && hasNoConflict)
	if !isValidWithNoConflict {
		if hasUniqueNameLabel {
			// The Service using the EndpointSlice is not valid for export or has conflicts with other exported
			// Services, but the EndpointSlice has a unique name label present (i.e. it might have been
			// exported before); the EndpointSlice should be unexported.
			return shouldUnexportEndpointSliceOp, nil
		}
		// The Service using the EndpointSlice is not valid for export or has conflicts with other exported
		// Services, and the EndpointSlice has no unique name label present (i.e. it has not been
		// exported before); the EndpointSlice should be skipped for further processing.
		return shouldSkipEndpointSliceOp, nil
	}

	if hasUniqueNameLabel && endpointSlice.DeletionTimestamp != nil {
		// The Service using the EndpointSlice is exported with no conflicts, and the EndpointSlice has a unique
		// name label (i.e. it might have been exported), but it has been deleted; as a result,
		// the EndpointSlice should be unexported.
		return shouldUnexportEndpointSliceOp, nil
	}

	// The Service using the EndpointSlice is exported with no conflicts, and the EndpointSlice is not marked
	// for deletion; the EndpointSlice should be further processed.
	return noSkipOrUnexportNeededOp, nil
}

// unexportEndpointSlice unexports an EndpointSlice by deleting its corresponding EndpointSliceExport.
func (r *Reconciler) unexportEndpointSlice(ctx context.Context, endpointSlice *discoveryv1.EndpointSlice) error {
	fleetUniqueName := endpointSlice.Labels[endpointSliceUniqueNameLabel]

	endpointSliceExport := fleetnetv1alpha1.EndpointSliceExport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.hubNamespace,
			Name:      fleetUniqueName,
		},
	}
	// It is guaranteed that a unique name label is always added before an EndpointSlice is exported; and
	// in some rare occasions it could happen that an EndpointSlice has a unique name label present yet has
	// not been exported to the hub cluster. It is an expected behavior and no action is needed on this controller's
	// end.
	if err := r.hubClient.Delete(ctx, &endpointSliceExport); err != nil && !errors.IsNotFound(err) {
		// An unexpected error has occurred.
		return err
	}

	// Remove the unique name label; this must happen after the EndpointSliceExport has been deleted.
	endpointSliceCopy := endpointSlice.DeepCopy()
	delete(endpointSliceCopy.Labels, endpointSliceUniqueNameLabel)
	return r.memberClient.Update(ctx, endpointSliceCopy)
}

// assignUniqueNameAsLabel assigns a new unique name as a label.
func (r *Reconciler) assignUniqueNameAsLabel(ctx context.Context, endpointSlice *discoveryv1.EndpointSlice) (string, error) {
	fleetUniqueName := formatFleetUniqueName(r.memberClusterID, endpointSlice)
	updatedEndpointSlice := endpointSlice.DeepCopy()
	// Initialize the labels field if no labels are present.
	if updatedEndpointSlice.Labels == nil {
		updatedEndpointSlice.Labels = map[string]string{}
	}
	updatedEndpointSlice.Labels[endpointSliceUniqueNameLabel] = fleetUniqueName
	err := r.memberClient.Update(ctx, updatedEndpointSlice)
	return fleetUniqueName, err
}
