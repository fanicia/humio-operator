/*
Copyright 2020 Humio https://humio.com

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"github.com/humio/humio-operator/pkg/kubernetes"
	"reflect"

	humioapi "github.com/humio/cli/api"

	"github.com/humio/humio-operator/pkg/helpers"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	humiov1alpha1 "github.com/humio/humio-operator/api/v1alpha1"
	"github.com/humio/humio-operator/pkg/humio"
)

// HumioAlertReconciler reconciles a HumioAlert object
type HumioAlertReconciler struct {
	client.Client
	BaseLogger  logr.Logger
	Log         logr.Logger
	HumioClient humio.Client
	Namespace   string
}

//+kubebuilder:rbac:groups=core.humio.com,resources=humioalerts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core.humio.com,resources=humioalerts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core.humio.com,resources=humioalerts/finalizers,verbs=update

func (r *HumioAlertReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Namespace != "" {
		if r.Namespace != req.Namespace {
			return reconcile.Result{}, nil
		}
	}

	r.Log = r.BaseLogger.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name, "Request.Type", helpers.GetTypeName(r), "Reconcile.ID", kubernetes.RandomString())
	r.Log.Info("Reconciling HumioAlert")

	ha := &humiov1alpha1.HumioAlert{}
	err := r.Get(ctx, req.NamespacedName, ha)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	r.Log = r.Log.WithValues("Request.UID", ha.UID)

	cluster, err := helpers.NewCluster(ctx, r, ha.Spec.ManagedClusterName, ha.Spec.ExternalClusterName, ha.Namespace, helpers.UseCertManager(), true)
	if err != nil || cluster == nil || cluster.Config() == nil {
		r.Log.Error(err, "unable to obtain humio client config")
		err = r.setState(ctx, humiov1alpha1.HumioAlertStateConfigError, ha)
		if err != nil {
			return reconcile.Result{}, r.logErrorAndReturn(err, "unable to set alert state")
		}
		return reconcile.Result{}, err
	}

	defer func(ctx context.Context, humioClient humio.Client, ha *humiov1alpha1.HumioAlert) {
		curAlert, err := r.HumioClient.GetAlert(cluster.Config(), req, ha)
		if errors.As(err, &humioapi.EntityNotFound{}) {
			_ = r.setState(ctx, humiov1alpha1.HumioAlertStateNotFound, ha)
			return
		}
		if err != nil || curAlert == nil {
			_ = r.setState(ctx, humiov1alpha1.HumioAlertStateConfigError, ha)
			return
		}
		_ = r.setState(ctx, humiov1alpha1.HumioAlertStateExists, ha)
	}(ctx, r.HumioClient, ha)

	return r.reconcileHumioAlert(ctx, cluster.Config(), ha, req)
}

func (r *HumioAlertReconciler) reconcileHumioAlert(ctx context.Context, config *humioapi.Config, ha *humiov1alpha1.HumioAlert, req ctrl.Request) (reconcile.Result, error) {
	// Delete
	r.Log.Info("Checking if alert is marked to be deleted")
	isMarkedForDeletion := ha.GetDeletionTimestamp() != nil
	if isMarkedForDeletion {
		r.Log.Info("Alert marked to be deleted")
		if helpers.ContainsElement(ha.GetFinalizers(), humioFinalizer) {
			// Run finalization logic for humioFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			r.Log.Info("Deleting alert")
			if err := r.HumioClient.DeleteAlert(config, req, ha); err != nil {
				return reconcile.Result{}, r.logErrorAndReturn(err, "Delete alert returned error")
			}

			r.Log.Info("Alert Deleted. Removing finalizer")
			ha.SetFinalizers(helpers.RemoveElement(ha.GetFinalizers(), humioFinalizer))
			err := r.Update(ctx, ha)
			if err != nil {
				return reconcile.Result{}, err
			}
			r.Log.Info("Finalizer removed successfully")
		}
		return reconcile.Result{}, nil
	}

	r.Log.Info("Checking if alert requires finalizer")
	// Add finalizer for this CR
	if !helpers.ContainsElement(ha.GetFinalizers(), humioFinalizer) {
		r.Log.Info("Finalizer not present, adding finalizer to alert")
		ha.SetFinalizers(append(ha.GetFinalizers(), humioFinalizer))
		err := r.Update(ctx, ha)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{Requeue: true}, nil
	}

	r.Log.Info("Checking if alert needs to be created")
	// Add Alert
	curAlert, err := r.HumioClient.GetAlert(config, req, ha)
	if errors.As(err, &humioapi.EntityNotFound{}) {
		r.Log.Info("Alert doesn't exist. Now adding alert")
		addedAlert, err := r.HumioClient.AddAlert(config, req, ha)
		if err != nil {
			return reconcile.Result{}, r.logErrorAndReturn(err, "could not create alert")
		}
		r.Log.Info("Created alert", "Alert", ha.Spec.Name)

		result, err := r.reconcileHumioAlertAnnotations(ctx, addedAlert, ha, req)
		if err != nil {
			return result, err
		}
		return reconcile.Result{Requeue: true}, nil
	}
	if err != nil {
		return reconcile.Result{}, r.logErrorAndReturn(err, "could not check if alert exists")
	}

	r.Log.Info("Checking if alert needs to be updated")
	// Update
	actionIdMap, err := r.HumioClient.GetActionIDsMapForAlerts(config, req, ha)
	if err != nil {
		return reconcile.Result{}, r.logErrorAndReturn(err, "could not get action id mapping")
	}
	expectedAlert, err := humio.AlertTransform(ha, actionIdMap)
	if err != nil {
		return reconcile.Result{}, r.logErrorAndReturn(err, "could not parse expected Alert")
	}

	sanitizeAlert(curAlert)
	if !reflect.DeepEqual(*curAlert, *expectedAlert) {
		r.Log.Info(fmt.Sprintf("Alert differs, triggering update, expected %#v, got: %#v",
			expectedAlert,
			curAlert))
		alert, err := r.HumioClient.UpdateAlert(config, req, ha)
		if err != nil {
			return reconcile.Result{}, r.logErrorAndReturn(err, "could not update alert")
		}
		if alert != nil {
			r.Log.Info(fmt.Sprintf("Updated alert %q", alert.Name))
		}
	}

	r.Log.Info("done reconciling, will requeue after 15 seconds")
	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HumioAlertReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&humiov1alpha1.HumioAlert{}).
		Complete(r)
}

func (r *HumioAlertReconciler) setState(ctx context.Context, state string, ha *humiov1alpha1.HumioAlert) error {
	if ha.Status.State == state {
		return nil
	}
	r.Log.Info(fmt.Sprintf("setting alert state to %s", state))
	ha.Status.State = state
	return r.Status().Update(ctx, ha)
}

func (r *HumioAlertReconciler) logErrorAndReturn(err error, msg string) error {
	r.Log.Error(err, msg)
	return fmt.Errorf("%s: %w", msg, err)
}

func sanitizeAlert(alert *humioapi.Alert) {
	alert.TimeOfLastTrigger = 0
	alert.ID = ""
	alert.LastError = ""
}
