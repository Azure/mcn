/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

// Package serviceexport features the ServiceExport controller for exporting a Service from a member cluster to
// its fleet.
package serviceexport

import (
	"context"
	"fmt"

	fleetnetworkingapi "go.goms.io/fleet-networking/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// ServiceExportReconciler reconciles the export of a Service.
type SvcExportReconciler struct {
	memberClient client.Client
	hubClient    client.Client
	hubNS        string
}

// TO-DO (chenyu1): Add RBAC markers.
// TO-DO (chenyu1): Add logs, events, and metrics + check their message formats.
// Reconcile exports a Service.
func (r *SvcExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Retrieve the ServiceExport object.
	var svcExport fleetnetworkingapi.ServiceExport
	if err := r.memberClient.Get(ctx, req.NamespacedName, &svcExport); err != nil {
		log.Error(err, "failed to get service export")
		// Skip the reconcilation if the ServiceExport does not exist; this should only happen when a ServiceExport
		// is deleted before the corresponding Service is exported to the fleet (and a cleanup finalizer is added),
		// which requires no action to take on this controller's end.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if the ServiceExport has been deleted and needs cleanup (unexporting Service).
	// A ServiceExport needs cleanup when it has the ServiceExport cleanup finalizer added; the absence of this
	// finalizer guarantees that the corresponding Service has never been exported to the fleet.
	if isSvcExportCleanupNeeded(&svcExport) {
		return r.unexportSvc(ctx, &svcExport)
	}

	// Check if the Service to export exists.
	var svc corev1.Service
	err := r.memberClient.Get(ctx, req.NamespacedName, &svc)
	switch {
	// The Service to export does not exist or has been deleted.
	case errors.IsNotFound(err) || isSvcDeleted(&svc):
		// Unexport the Service if the ServiceExport has the cleanup finalizer added.
		if hasSvcExportCleanupFinalizer(&svcExport) {
			if _, err = r.unexportSvc(ctx, &svcExport); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Mark the ServiceExport as invalid.
		return r.markSvcExportAsInvalidSvcNotFound(ctx, &svcExport)
	// An unexpected error occurs when retrieving the Service.
	case err != nil:
		return ctrl.Result{}, err
	}

	// Check if the Service is eligible for export.
	if !isSvcEligibleForExport(&svc) {
		// Unexport ineligible Service if the ServiceExport has the cleanup finalizer added.
		if hasSvcExportCleanupFinalizer(&svcExport) {
			if _, err = r.unexportSvc(ctx, &svcExport); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Mark the ServiceExport as invalid.
		return r.markSvcExportAsInvalidSvcIneligible(ctx, &svcExport)
	}

	// Add the cleanup finalizer; this must happen before the Service is actually exported.
	if !hasSvcExportCleanupFinalizer(&svcExport) {
		_, err = r.addCleanupFinalizer(ctx, &svcExport)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Export the Service or update the exported Service.

	// Retrieve the InternalServiceExport object.
	var internalSvcExport fleetnetworkingapi.InternalServiceExport
	internalSvcExportKey := client.ObjectKey{Namespace: r.hubNS, Name: formatInternalSvcExportName(&svcExport)}
	err = r.hubClient.Get(ctx, internalSvcExportKey, &internalSvcExport)
	updateInternalSvcExport(&svc, &svcExport)
	if errors.IsNotFound(err) {
		// Export the Service
		err = r.hubClient.Create(ctx, &internalSvcExport)
	} else {
		// Update the exported Service
		err = r.hubClient.Update(ctx, &internalSvcExport)
	}
	return ctrl.Result{}, err
}

// SetupWithManager builds a controller with SvcExportReconciler and sets it up with a controller manager.
func (r *SvcExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	svcExportHandlerFuncs := handler.Funcs{
		CreateFunc: func(e event.CreateEvent, q workqueue.RateLimitingInterface) {
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: e.Object.GetNamespace(),
					Name:      e.Object.GetName(),
				},
			})
		},
		UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			// Enqueue a ServiceExport for processing only when it is deleted; other changes can be ignored.
			oldObj := e.ObjectOld
			newObj := e.ObjectNew
			_, oldOk := oldObj.(*fleetnetworkingapi.ServiceExport)
			_, newOk := newObj.(*fleetnetworkingapi.ServiceExport)
			if oldOk && newOk && newObj.GetDeletionTimestamp() != nil {
				q.Add(reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: newObj.GetNamespace(),
						Name:      newObj.GetName(),
					},
				})
			}
		},
		GenericFunc: func(e event.GenericEvent, q workqueue.RateLimitingInterface) {
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: e.Object.GetNamespace(),
					Name:      e.Object.GetName(),
				},
			})
		},
		// Delete events are ignored; deleted ServiceExports are reconciled already when the DeletionTimestamp is
		// added.
	}

	svcHandlerFuncs := handler.Funcs{
		CreateFunc: func(e event.CreateEvent, q workqueue.RateLimitingInterface) {
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: e.Object.GetNamespace(),
					Name:      e.Object.GetName(),
				},
			})
		},
		UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			// Enqueue a Service for processing only when its spec has changed in a significant way or it has been
			// deleted; this helps filter out some update events as many Service spec fields are not exported at all.
			oldObj := e.ObjectOld
			newObj := e.ObjectNew
			oldSvc, oldOk := oldObj.(*corev1.Service)
			newSvc, newOk := newObj.(*corev1.Service)
			if oldOk && newOk && (isSvcChanged(oldSvc, newSvc) || isSvcDeleted(newSvc)) {
				q.Add(reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: newObj.GetNamespace(),
						Name:      newObj.GetName(),
					},
				})
			}
		},
		DeleteFunc: func(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: e.Object.GetNamespace(),
					Name:      e.Object.GetName(),
				},
			})
		},
		GenericFunc: func(e event.GenericEvent, q workqueue.RateLimitingInterface) {
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: e.Object.GetNamespace(),
					Name:      e.Object.GetName(),
				},
			})
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		// The ServiceExport controller should watch over ServiceExport objects.
		Watches(&source.Kind{Type: &fleetnetworkingapi.ServiceExport{}}, svcExportHandlerFuncs).
		// The ServiceExport controller should watch over Service objects.
		Watches(&source.Kind{Type: &corev1.Service{}}, svcHandlerFuncs).
		Complete(r)
}

func (r *SvcExportReconciler) unexportSvc(ctx context.Context, svcExport *fleetnetworkingapi.ServiceExport) (ctrl.Result, error) {
	// Get the unique name assigned when the Service is exported. it is guaranteed that Services are
	// always exported using the name format `ORIGINAL_NAMESPACE-ORIGINAL_NAME`; for example, a Service
	// from namespace `default`` with the name `store`` will be exported with the name `default-store`.
	internalSvcExportName := formatInternalSvcExportName(svcExport)

	internalSvcExport := &fleetnetworkingapi.InternalServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.hubNS,
			Name:      internalSvcExportName,
		},
	}

	// Unexport the Service.
	if err := r.hubClient.Delete(ctx, internalSvcExport); err != nil {
		// It is guaranteed that a finalizer is always added before the Service is actually exported; as a result,
		// in some rare occasions it could happen that a ServiceExport has a finalizer present yet the corresponding
		// Service has not actually been exported to the hub cluster. It is an expected behavior and no action
		// is needed on this controller's end.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Remove the finalizer; it must happen after the Service has successfully been unexported.
	return r.removeSvcExportCleanupFinalizer(ctx, svcExport)
}

// removeSvcExportCleanupFinalizer removes the cleanup finalizer from a ServiceExport.
func (r *SvcExportReconciler) removeSvcExportCleanupFinalizer(ctx context.Context, svcExport *fleetnetworkingapi.ServiceExport) (ctrl.Result, error) {
	updatedFinalizers := []string{}
	for _, finalizer := range svcExport.GetFinalizers() {
		if finalizer != svcExportCleanupFinalizer {
			updatedFinalizers = append(updatedFinalizers, finalizer)
		}
	}
	svcExport.SetFinalizers(updatedFinalizers)
	err := r.memberClient.Update(ctx, svcExport)
	return ctrl.Result{}, err
}

// markSvcExportAsInvalidNotFound marks a ServiceExport as invalid.
func (r *SvcExportReconciler) markSvcExportAsInvalidSvcNotFound(ctx context.Context, svcExport *fleetnetworkingapi.ServiceExport) (ctrl.Result, error) {
	updatedConds := []metav1.Condition{}
	for _, cond := range svcExport.Status.Conditions {
		if cond.Type != string(fleetnetworkingapi.ServiceExportValid) {
			updatedConds = append(updatedConds, cond)
		}
	}
	updatedConds = append(updatedConds, metav1.Condition{
		Type:               string(fleetnetworkingapi.ServiceExportValid),
		Status:             metav1.ConditionStatus(corev1.ConditionFalse),
		LastTransitionTime: metav1.Now(),
		Reason:             "ServiceNotFound",
		Message:            fmt.Sprintf("service %s/%s is not found", svcExport.Namespace, svcExport.Name),
	})
	svcExport.Status.Conditions = updatedConds
	err := r.memberClient.Update(ctx, svcExport)
	return ctrl.Result{}, err
}

// markSvcExportAsInvalidSvcIneligible marks a ServiceExport as invalid.
func (r *SvcExportReconciler) markSvcExportAsInvalidSvcIneligible(ctx context.Context, svcExport *fleetnetworkingapi.ServiceExport) (ctrl.Result, error) {
	updatedConds := []metav1.Condition{}
	for _, cond := range svcExport.Status.Conditions {
		if cond.Type != string(fleetnetworkingapi.ServiceExportValid) {
			updatedConds = append(updatedConds, cond)
		}
	}
	updatedConds = append(updatedConds, metav1.Condition{
		Type:               string(fleetnetworkingapi.ServiceExportValid),
		Status:             metav1.ConditionStatus(corev1.ConditionFalse),
		LastTransitionTime: metav1.Now(),
		Reason:             "ServiceIneligible",
		Message:            fmt.Sprintf("service %s/%s is not eligible for export", svcExport.Namespace, svcExport.Name),
	})
	svcExport.Status.Conditions = updatedConds
	err := r.memberClient.Update(ctx, svcExport)
	return ctrl.Result{}, err
}

// addCleanupFinalizer adds the cleanup finalizer to a ServiceExport.
func (r *SvcExportReconciler) addCleanupFinalizer(ctx context.Context, svcExport *fleetnetworkingapi.ServiceExport) (ctrl.Result, error) {
	updatedFinalizers := svcExport.GetFinalizers()
	updatedFinalizers = append(updatedFinalizers, svcExportCleanupFinalizer)
	svcExport.SetFinalizers(updatedFinalizers)
	err := r.memberClient.Update(ctx, svcExport)
	return ctrl.Result{}, err
}
