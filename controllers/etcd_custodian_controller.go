// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"fmt"
	"time"

	extensionshandler "github.com/gardener/gardener/extensions/pkg/handler"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	druidmapper "github.com/gardener/etcd-druid/pkg/mapper"
	druidpredicates "github.com/gardener/etcd-druid/pkg/predicate"
)

// EtcdCustodian reconciles status of Etcd object
type EtcdCustodian struct {
	client.Client
	Scheme *runtime.Scheme
	logger logr.Logger
}

// NewEtcdCustodian creates a new EtcdCustodian object
func NewEtcdCustodian(mgr manager.Manager) *EtcdCustodian {
	return &EtcdCustodian{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		logger: log.Log.WithName("custodian-controller"),
	}
}

// +kubebuilder:rbac:groups=druid.gardener.cloud,resources=etcds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=druid.gardener.cloud,resources=etcds/status,verbs=get;update;patch

// Reconcile reconciles the etcd.
func (ec *EtcdCustodian) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ec.logger.Info("Custodian controller reconciliation started")
	etcd := &druidv1alpha1.Etcd{}
	if err := ec.Get(ctx, req.NamespacedName, etcd); err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return. Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	logger := ec.logger.WithValues("etcd", kutil.Key(etcd.Namespace, etcd.Name).String())

	// TODO: (timuthy) remove this as it could block important health checks
	if etcd.Status.LastError != nil && *etcd.Status.LastError != "" {
		logger.Info(fmt.Sprintf("Requeue item because of last error: %v", *etcd.Status.LastError))
		return ctrl.Result{
			RequeueAfter: 30 * time.Second,
		}, nil
	}

	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read some time after listing Machines (see #42639).
	canAdoptFunc := RecheckDeletionTimestamp(func() (metav1.Object, error) {
		foundEtcd := &druidv1alpha1.Etcd{}
		err := ec.Get(ctx, types.NamespacedName{Name: etcd.Name, Namespace: etcd.Namespace}, foundEtcd)
		if err != nil {
			return nil, err
		}

		if foundEtcd.GetDeletionTimestamp() != nil {
			return nil, fmt.Errorf("%v/%v etcd is marked for deletion", etcd.Namespace, etcd.Name)
		}
		if foundEtcd.UID != etcd.UID {
			return nil, fmt.Errorf("original %v/%v etcd gone: got uid %v, wanted %v", etcd.Namespace, etcd.Name, foundEtcd.UID, etcd.UID)
		}
		return foundEtcd, nil
	})

	selector, err := metav1.LabelSelectorAsSelector(etcd.Spec.Selector)
	if err != nil {
		logger.Error(err, "Error converting etcd selector to selector")
		return ctrl.Result{}, err
	}

	refMgr := NewEtcdDruidRefManager(ec.Client, ec.Scheme, etcd, selector, etcdGVK, canAdoptFunc)

	stsList, err := refMgr.FetchStatefulSet(ctx, etcd)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Requeue if we found more than one or no StatefulSet.
	// The Etcd controller needs to decide what to do in such situations.
	if len(stsList.Items) != 1 {
		ec.updateEtcdStatusWithNoSts(ctx, logger, etcd)
		return ctrl.Result{
			RequeueAfter: 5 * time.Second,
		}, nil
	}

	if err := ec.updateEtcdStatus(ctx, logger, etcd, &stsList.Items[0]); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (ec *EtcdCustodian) updateEtcdStatus(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet) error {
	logger.Info("Updating etcd status with statefulset information")

	return kutil.TryUpdateStatus(ctx, retry.DefaultBackoff, ec.Client, etcd, func() error {
		etcd.Status.Etcd = &druidv1alpha1.CrossVersionObjectReference{
			APIVersion: sts.APIVersion,
			Kind:       sts.Kind,
			Name:       sts.Name,
		}
		ready := CheckStatefulSet(etcd, sts) == nil

		// To be changed once we have multiple replicas.
		etcd.Status.CurrentReplicas = sts.Status.CurrentReplicas
		etcd.Status.ReadyReplicas = sts.Status.ReadyReplicas
		etcd.Status.UpdatedReplicas = sts.Status.UpdatedReplicas
		etcd.Status.Ready = &ready
		logger.Info(fmt.Sprintf("ETCD status updated for statefulset current replicas: %v, ready replicas: %v, updated replicas: %v", sts.Status.CurrentReplicas, sts.Status.ReadyReplicas, sts.Status.UpdatedReplicas))
		return nil
	})
}

func (ec *EtcdCustodian) updateEtcdStatusWithNoSts(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd) {
	logger.Info("Updating etcd status when no statefulset found")

	if err := kutil.TryUpdateStatus(ctx, retry.DefaultBackoff, ec.Client, etcd, func() error {
		// TODO: (timuthy) Don't reset all conditions as some of them will be maintained by other actors (e.g. etcd-backup-restore)
		conditions := []druidv1alpha1.Condition{}
		etcd.Status.Conditions = conditions

		// To be changed once we have multiple replicas.
		etcd.Status.CurrentReplicas = 0
		etcd.Status.ReadyReplicas = 0
		etcd.Status.UpdatedReplicas = 0

		etcd.Status.Ready = pointer.BoolPtr(false)
		return nil
	}); err != nil {
		logger.Error(err, "Error while updating ETCD status when no statefulset found")
	}
}

// SetupWithManager sets up manager with a new controller and ec as the reconcile.Reconciler
func (ec *EtcdCustodian) SetupWithManager(ctx context.Context, mgr ctrl.Manager, workers int) error {
	builder := ctrl.NewControllerManagedBy(mgr).WithOptions(controller.Options{
		MaxConcurrentReconciles: workers,
	})

	return builder.
		For(&druidv1alpha1.Etcd{}).
		Watches(
			&source.Kind{Type: &appsv1.StatefulSet{}},
			extensionshandler.EnqueueRequestsFromMapper(druidmapper.StatefulSetToEtcd(ctx, mgr.GetClient()), extensionshandler.UpdateWithNew),
			ctrlbuilder.WithPredicates(druidpredicates.StatefulSetStatusChange()),
		).
		Complete(ec)
}
