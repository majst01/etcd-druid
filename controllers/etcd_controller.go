// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/etcd-druid/pkg/chartrenderer"
	"github.com/gardener/etcd-druid/pkg/client/kubernetes"
	"github.com/gardener/etcd-druid/pkg/common"
	druidpredicates "github.com/gardener/etcd-druid/pkg/predicate"
	"github.com/gardener/etcd-druid/pkg/utils"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	gardenerretry "github.com/gardener/gardener/pkg/utils/retry"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	eventsv1beta1 "k8s.io/api/events/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var (
	etcdGVK = druidv1alpha1.GroupVersion.WithKind("Etcd")

	// UncachedObjectList is a list of objects which should not be cached.
	UncachedObjectList = []client.Object{
		&corev1.Event{},
		&eventsv1beta1.Event{},
		&eventsv1.Event{},
	}
)

const (
	// FinalizerName is the name of the Plant finalizer.
	FinalizerName = "druid.gardener.cloud/etcd-druid"
	// DefaultImageVector is a constant for the path to the default image vector file.
	DefaultImageVector = "images.yaml"
	// DefaultInterval is the default interval for retry operations.
	DefaultInterval = 5 * time.Second
	// EtcdReady implies that etcd is ready
	EtcdReady = true
	// DefaultAutoCompactionRetention defines the default auto-compaction-retention length for etcd.
	DefaultAutoCompactionRetention = "30m"
)

var (
	// DefaultTimeout is the default timeout for retry operations.
	DefaultTimeout = 1 * time.Minute
)

// EtcdReconciler reconciles a Etcd object
type EtcdReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	chartApplier kubernetes.ChartApplier
	Config       *rest.Config
	ImageVector  imagevector.ImageVector
	logger       logr.Logger
}

// NewReconcilerWithImageVector creates a new EtcdReconciler object with an image vector
func NewReconcilerWithImageVector(mgr manager.Manager) (*EtcdReconciler, error) {
	etcdReconciler, err := NewEtcdReconciler(mgr)
	if err != nil {
		return nil, err
	}
	return etcdReconciler.InitializeControllerWithImageVector()
}

// NewEtcdReconciler creates a new EtcdReconciler object
func NewEtcdReconciler(mgr manager.Manager) (*EtcdReconciler, error) {
	return (&EtcdReconciler{
		Client: mgr.GetClient(),
		Config: mgr.GetConfig(),
		Scheme: mgr.GetScheme(),
		logger: log.Log.WithName("etcd-controller"),
	}).InitializeControllerWithChartApplier()
}

// NewEtcdReconcilerWithImageVector creates a new EtcdReconciler object
func NewEtcdReconcilerWithImageVector(mgr manager.Manager) (*EtcdReconciler, error) {
	ec, err := NewEtcdReconciler(mgr)
	if err != nil {
		return nil, err
	}
	return ec.InitializeControllerWithImageVector()
}

func getChartPath() string {
	return filepath.Join("charts", "etcd")
}

func getChartPathForStatefulSet() string {
	return filepath.Join("etcd", "templates", "etcd-statefulset.yaml")
}

func getChartPathForConfigMap() string {
	return filepath.Join("etcd", "templates", "etcd-configmap.yaml")
}

func getChartPathForService() string {
	return filepath.Join("etcd", "templates", "etcd-service.yaml")
}

func getImageYAMLPath() string {
	return filepath.Join(common.ChartPath, DefaultImageVector)
}

// InitializeControllerWithChartApplier will use EtcdReconciler client to initialize a Kubernetes client as well as
// a Chart renderer.
func (r *EtcdReconciler) InitializeControllerWithChartApplier() (*EtcdReconciler, error) {
	if r.chartApplier != nil {
		return r, nil
	}

	renderer, err := chartrenderer.NewForConfig(r.Config)
	if err != nil {
		return nil, err
	}
	applier, err := kubernetes.NewApplierForConfig(r.Config)
	if err != nil {
		return nil, err
	}
	r.chartApplier = kubernetes.NewChartApplier(renderer, applier)
	return r, nil
}

// InitializeControllerWithImageVector will use EtcdReconciler client to initialize image vector for etcd
// and backup restore images.
func (r *EtcdReconciler) InitializeControllerWithImageVector() (*EtcdReconciler, error) {
	imageVector, err := imagevector.ReadGlobalImageVectorWithEnvOverride(getImageYAMLPath())
	if err != nil {
		return nil, err
	}
	r.ImageVector = imageVector
	return r, nil
}

// +kubebuilder:rbac:groups=druid.gardener.cloud,resources=etcds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=druid.gardener.cloud,resources=etcds/status,verbs=get;update;patch

// Reconcile reconciles the etcd.
func (r *EtcdReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	etcd := &druidv1alpha1.Etcd{}
	if err := r.Get(ctx, req.NamespacedName, etcd); err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return. Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	if !etcd.DeletionTimestamp.IsZero() {
		return r.delete(ctx, etcd)
	}
	return r.reconcile(ctx, etcd)
}

func (r *EtcdReconciler) reconcile(ctx context.Context, etcd *druidv1alpha1.Etcd) (ctrl.Result, error) {
	logger := r.logger.WithValues("etcd", kutil.Key(etcd.Namespace, etcd.Name).String(), "operation", "reconcile")
	logger.Info("Starting operation")

	// Add Finalizers to Etcd
	if finalizers := sets.NewString(etcd.Finalizers...); !finalizers.Has(FinalizerName) {
		logger.Info("Adding finalizer")
		finalizers.Insert(FinalizerName)
		etcd.Finalizers = finalizers.UnsortedList()
		if err := r.Update(ctx, etcd); err != nil {
			if err := r.updateEtcdErrorStatus(ctx, etcd, nil, err); err != nil {
				return ctrl.Result{
					Requeue: true,
				}, err
			}
			return ctrl.Result{
				Requeue: true,
			}, err
		}
	}
	if err := r.addFinalizersToDependantSecrets(ctx, logger, etcd); err != nil {
		if err := r.updateEtcdErrorStatus(ctx, etcd, nil, err); err != nil {
			return ctrl.Result{
				Requeue: true,
			}, err
		}
	}
	etcd, err := r.updateEtcdStatusAsNotReady(ctx, etcd)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{
			Requeue: true,
		}, err
	}
	svc, ss, err := r.reconcileEtcd(ctx, logger, etcd)
	if err != nil {
		if err := r.updateEtcdErrorStatus(ctx, etcd, ss, err); err != nil {
			logger.Error(err, "Error during reconciling ETCD")
			return ctrl.Result{
				Requeue: true,
			}, err
		}
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	if err := r.updateEtcdStatus(ctx, etcd, svc, ss); err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	return ctrl.Result{
		Requeue: false,
	}, nil
}

func (r *EtcdReconciler) delete(ctx context.Context, etcd *druidv1alpha1.Etcd) (ctrl.Result, error) {
	logger := r.logger.WithValues("etcd", kutil.Key(etcd.Namespace, etcd.Name).String(), "operation", "delete")
	logger.Info("Starting operation")

	if err := r.removeDependantStatefulset(ctx, logger, etcd); err != nil {
		if err := r.updateEtcdErrorStatus(ctx, etcd, nil, err); err != nil {
			return ctrl.Result{
				Requeue: true,
			}, err
		}
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	if err := r.removeFinalizersToDependantSecrets(ctx, logger, etcd); err != nil {
		if err := r.updateEtcdErrorStatus(ctx, etcd, nil, err); err != nil {
			return ctrl.Result{
				Requeue: true,
			}, err
		}
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	if sets.NewString(etcd.Finalizers...).Has(FinalizerName) {
		logger.Info("Removing finalizer")

		// Deep copy of etcd resource required here to patch the object. Update call results in
		// StorageError. See also: https://github.com/kubernetes/kubernetes/issues/71139
		etcdCopy := etcd.DeepCopy()
		finalizers := sets.NewString(etcdCopy.Finalizers...)
		finalizers.Delete(FinalizerName)
		etcdCopy.Finalizers = finalizers.UnsortedList()
		if err := r.Patch(ctx, etcdCopy, client.MergeFrom(etcd)); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{
				Requeue: true,
			}, err
		}
	}
	logger.Info("Deleted etcd successfully.")
	return ctrl.Result{}, nil
}

func (r *EtcdReconciler) reconcileServices(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd, renderedChart *chartrenderer.RenderedChart) (*corev1.Service, error) {
	logger.Info("Reconciling etcd services")

	selector, err := metav1.LabelSelectorAsSelector(etcd.Spec.Selector)
	if err != nil {
		logger.Error(err, "Error converting etcd selector to selector")
		return nil, err
	}

	// list all services to include the services that don't match the etcd`s selector
	// anymore but has the stale controller ref.
	services := &corev1.ServiceList{}
	err = r.List(ctx, services, client.InNamespace(etcd.Namespace), client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		logger.Error(err, "Error listing services")
		return nil, err
	}

	// NOTE: filteredStatefulSets are pointing to deepcopies of the cache, but this could change in the future.
	// Ref: https://github.com/kubernetes-sigs/controller-runtime/blob/release-0.2/pkg/cache/internal/cache_reader.go#L74
	// if you need to modify them, you need to copy it first.
	filteredServices, err := r.claimServices(ctx, etcd, selector, services)
	if err != nil {
		logger.Error(err, "Error claiming service")
		return nil, err
	}

	if len(filteredServices) > 0 {
		logger.Info("Claiming existing etcd services")

		// Keep only 1 Service. Delete the rest
		for i := 1; i < len(filteredServices); i++ {
			ss := filteredServices[i]
			if err := r.Delete(ctx, ss); err != nil {
				logger.Error(err, "Error in deleting duplicate StatefulSet")
				continue
			}
		}

		// Return the updated Service
		service := &corev1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: filteredServices[0].Name, Namespace: filteredServices[0].Namespace}, service)
		if err != nil {
			return nil, err
		}

		// Service is claimed by for this etcd. Just sync the specs
		if service, err = r.syncServiceSpec(ctx, logger, service, etcd, renderedChart); err != nil {
			return nil, err
		}

		return service, err
	}

	// Required Service doesn't exist. Create new

	svc, err := r.getServiceFromEtcd(etcd, renderedChart)
	if err != nil {
		return nil, err
	}

	err = r.Create(ctx, svc)

	// Ignore the precondition violated error, this service is already updated
	// with the desired label.
	if err == errorsutil.ErrPreconditionViolated {
		logger.Info("Service precondition doesn't hold, skip updating it.", "service", kutil.Key(svc.Namespace, svc.Name).String())
		err = nil
	}

	if err != nil {
		return nil, err
	}

	if err := controllerutil.SetControllerReference(etcd, svc, r.Scheme); err != nil {
		return nil, err
	}

	return svc.DeepCopy(), err
}

func (r *EtcdReconciler) syncServiceSpec(ctx context.Context, logger logr.Logger, svc *corev1.Service, etcd *druidv1alpha1.Etcd, renderedChart *chartrenderer.RenderedChart) (*corev1.Service, error) {
	decoded, err := r.getServiceFromEtcd(etcd, renderedChart)
	if err != nil {
		return nil, err
	}

	if reflect.DeepEqual(svc.Spec, decoded.Spec) {
		return svc, nil
	}
	svcCopy := svc.DeepCopy()
	decoded.Spec.DeepCopyInto(&svcCopy.Spec)
	// Copy ClusterIP as the field is immutable
	svcCopy.Spec.ClusterIP = svc.Spec.ClusterIP

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		return r.Patch(ctx, svcCopy, client.MergeFrom(svc))
	})

	// Ignore the precondition violated error, this machine is already updated
	// with the desired label.
	if err == errorsutil.ErrPreconditionViolated {
		logger.Info("Service precondition doesn't hold, skip updating it.", "service", kutil.Key(svc.Namespace, svc.Name).String())
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return svcCopy, err
}

func (r *EtcdReconciler) getServiceFromEtcd(etcd *druidv1alpha1.Etcd, renderedChart *chartrenderer.RenderedChart) (*corev1.Service, error) {
	var err error
	decoded := &corev1.Service{}
	servicePath := getChartPathForService()
	if _, ok := renderedChart.Files()[servicePath]; !ok {
		return nil, fmt.Errorf("missing service template file in the charts: %v", servicePath)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(renderedChart.Files()[servicePath])), 1024)

	if err = decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (r *EtcdReconciler) reconcileConfigMaps(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd, renderedChart *chartrenderer.RenderedChart) (*corev1.ConfigMap, error) {
	logger.Info("Reconciling etcd configmap")

	selector, err := metav1.LabelSelectorAsSelector(etcd.Spec.Selector)
	if err != nil {
		logger.Error(err, "Error converting etcd selector to selector")
		return nil, err
	}

	// list all configmaps to include the configmaps that don't match the etcd`s selector
	// anymore but has the stale controller ref.
	cms := &corev1.ConfigMapList{}
	err = r.List(ctx, cms, client.InNamespace(etcd.Namespace), client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		logger.Error(err, "Error listing configmaps")
		return nil, err
	}

	// NOTE: filteredCMs are pointing to deepcopies of the cache, but this could change in the future.
	// Ref: https://github.com/kubernetes-sigs/controller-runtime/blob/release-0.2/pkg/cache/internal/cache_reader.go#L74
	// if you need to modify them, you need to copy it first.
	filteredCMs, err := r.claimConfigMaps(ctx, etcd, selector, cms)
	if err != nil {
		return nil, err
	}

	if len(filteredCMs) > 0 {
		logger.Info("Claiming existing etcd configmaps")

		// Keep only 1 Configmap. Delete the rest
		for i := 1; i < len(filteredCMs); i++ {
			ss := filteredCMs[i]
			if err := r.Delete(ctx, ss); err != nil {
				logger.Error(err, "Error in deleting duplicate StatefulSet")
				continue
			}
		}

		// Return the updated Configmap
		cm := &corev1.ConfigMap{}
		err = r.Get(ctx, types.NamespacedName{Name: filteredCMs[0].Name, Namespace: filteredCMs[0].Namespace}, cm)
		if err != nil {
			return nil, err
		}

		// ConfigMap is claimed by for this etcd. Just sync the data
		if cm, err = r.syncConfigMapData(ctx, logger, cm, etcd, renderedChart); err != nil {
			return nil, err
		}

		return cm, err
	}

	// Required Configmap doesn't exist. Create new

	cm, err := r.getConfigMapFromEtcd(etcd, renderedChart)
	if err != nil {
		return nil, err
	}

	err = r.Create(ctx, cm)

	// Ignore the precondition violated error, this machine is already updated
	// with the desired label.
	if err == errorsutil.ErrPreconditionViolated {
		logger.Info("ConfigMap precondition doesn't hold, skip updating it.", "configmap", kutil.Key(cm.Namespace, cm.Name).String())
		err = nil
	}

	if err != nil {
		return nil, err
	}

	if err := controllerutil.SetControllerReference(etcd, cm, r.Scheme); err != nil {
		return nil, err
	}

	return cm.DeepCopy(), err
}

func (r *EtcdReconciler) syncConfigMapData(ctx context.Context, logger logr.Logger, cm *corev1.ConfigMap, etcd *druidv1alpha1.Etcd, renderedChart *chartrenderer.RenderedChart) (*corev1.ConfigMap, error) {
	decoded, err := r.getConfigMapFromEtcd(etcd, renderedChart)
	if err != nil {
		return nil, err
	}

	if reflect.DeepEqual(cm.Data, decoded.Data) {
		return cm, nil
	}
	cmCopy := cm.DeepCopy()
	cmCopy.Data = decoded.Data

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		return r.Patch(ctx, cmCopy, client.MergeFrom(cm))
	})

	// Ignore the precondition violated error, this machine is already updated
	// with the desired label.
	if err == errorsutil.ErrPreconditionViolated {
		logger.Info("ConfigMap precondition doesn't hold, skip updating it.", "configmap", kutil.Key(cm.Namespace, cm.Name).String())
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return cmCopy, err
}

func (r *EtcdReconciler) getConfigMapFromEtcd(etcd *druidv1alpha1.Etcd, renderedChart *chartrenderer.RenderedChart) (*corev1.ConfigMap, error) {
	var err error
	decoded := &corev1.ConfigMap{}
	configMapPath := getChartPathForConfigMap()

	if _, ok := renderedChart.Files()[configMapPath]; !ok {
		return nil, fmt.Errorf("missing configmap template file in the charts: %v", configMapPath)
	}

	//logger.Infof("%v: %v", statefulsetPath, renderer.Files()[statefulsetPath])
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(renderedChart.Files()[configMapPath])), 1024)

	if err = decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (r *EtcdReconciler) reconcileStatefulSet(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd, values map[string]interface{}) (*appsv1.StatefulSet, error) {
	logger.Info("Reconciling etcd statefulset")

	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing Machines (see #42639).
	canAdoptFunc := RecheckDeletionTimestamp(func() (metav1.Object, error) {
		foundEtcd := &druidv1alpha1.Etcd{}
		err := r.Get(context.TODO(), types.NamespacedName{Name: etcd.Name, Namespace: etcd.Namespace}, foundEtcd)
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
		return nil, err
	}
	dm := NewEtcdDruidRefManager(r.Client, r.Scheme, etcd, selector, etcdGVK, canAdoptFunc)
	statefulSets, err := dm.FetchStatefulSet(ctx, etcd)
	if err != nil {
		logger.Error(err, "Error while fetching StatefulSet")
		return nil, err
	}

	logger.Info("Claiming existing etcd StatefulSet")
	claimedStatefulSets, err := dm.ClaimStatefulsets(ctx, statefulSets)
	if err != nil {
		return nil, err
	}

	if len(claimedStatefulSets) > 0 {
		// Keep only 1 statefulset. Delete the rest
		for i := 1; i < len(claimedStatefulSets); i++ {
			sts := claimedStatefulSets[i]
			logger.Info("Found duplicate StatefulSet, deleting it", "statefulset", kutil.Key(sts.Namespace, sts.Name).String())
			if err := r.Delete(ctx, sts); err != nil {
				logger.Error(err, "Error in deleting duplicate StatefulSet", "statefulset", kutil.Key(sts.Namespace, sts.Name).String())
				continue
			}
		}

		// Fetch the updated statefulset
		// TODO: (timuthy) Check if this is really needed.
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: claimedStatefulSets[0].Name, Namespace: claimedStatefulSets[0].Namespace}, sts); err != nil {
			return nil, err
		}

		// Statefulset is claimed by for this etcd. Just sync the specs
		if sts, err = r.syncStatefulSetSpec(ctx, logger, sts, etcd, values); err != nil {
			return nil, err
		}

		// restart etcd pods in crashloop backoff
		selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
		if err != nil {
			logger.Error(err, "error converting StatefulSet selector to selector")
			return nil, err
		}
		podList := &v1.PodList{}
		if err := r.List(ctx, podList, client.InNamespace(etcd.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, err
		}

		for _, pod := range podList.Items {
			if utils.IsPodInCrashloopBackoff(pod.Status) {
				if err := r.Delete(ctx, &pod); err != nil {
					logger.Error(err, fmt.Sprintf("error deleting etcd pod in crashloop: %s/%s", pod.Namespace, pod.Name))
					return nil, err
				}
			}
		}

		return r.waitUntilStatefulSetReady(ctx, logger, etcd, sts)
	}

	// Required statefulset doesn't exist. Create new
	sts, err := r.getStatefulSetFromEtcd(etcd, values)
	if err != nil {
		return nil, err
	}

	err = r.Create(ctx, sts)

	// Ignore the precondition violated error, this machine is already updated
	// with the desired label.
	if err == errorsutil.ErrPreconditionViolated {
		logger.Info("StatefulSet %s precondition doesn't hold, skip updating it.", "statefulset", kutil.Key(sts.Namespace, sts.Name).String())
		err = nil
	}
	if err != nil {
		return nil, err
	}

	return r.waitUntilStatefulSetReady(ctx, logger, etcd, sts)
}

func getContainerMapFromPodTemplateSpec(spec v1.PodSpec) map[string]v1.Container {
	containers := map[string]v1.Container{}
	for _, c := range spec.Containers {
		containers[c.Name] = c
	}
	return containers
}

func (r *EtcdReconciler) syncStatefulSetSpec(ctx context.Context, logger logr.Logger, ss *appsv1.StatefulSet, etcd *druidv1alpha1.Etcd, values map[string]interface{}) (*appsv1.StatefulSet, error) {
	decoded, err := r.getStatefulSetFromEtcd(etcd, values)
	if err != nil {
		return nil, err
	}

	if reflect.DeepEqual(ss.Spec, decoded.Spec) {
		return ss, nil
	}

	ssCopy := ss.DeepCopy()
	ssCopy.Spec.Replicas = decoded.Spec.Replicas
	ssCopy.Spec.UpdateStrategy = decoded.Spec.UpdateStrategy

	recreateSTS := false
	if !reflect.DeepEqual(ssCopy.Spec.Selector, decoded.Spec.Selector) {
		recreateSTS = true
	}

	// Applying suggestions from
	containers := getContainerMapFromPodTemplateSpec(ssCopy.Spec.Template.Spec)
	for i, c := range decoded.Spec.Template.Spec.Containers {
		container, ok := containers[c.Name]
		if !ok {
			return nil, fmt.Errorf("container with name %s could not be fetched from statefulset %s", c.Name, decoded.Name)
		}
		decoded.Spec.Template.Spec.Containers[i].Resources = container.Resources
	}

	ssCopy.Spec.Template = decoded.Spec.Template

	if recreateSTS {
		logger.Info("Selector changed, recreating statefulset", "statefulset", kutil.Key(ssCopy.Namespace, ssCopy.Name).String())
		err = r.recreateStatefulset(ctx, decoded)
	} else {
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			return r.Patch(ctx, ssCopy, client.MergeFrom(ss))
		})
	}

	// Ignore the precondition violated error, this machine is already updated
	// with the desired label.
	if err == errorsutil.ErrPreconditionViolated {
		logger.Info("StatefulSet precondition doesn't hold, skip updating it", "statefulset", kutil.Key(ss.Namespace, ss.Name).String())
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return ssCopy, err
}

func (r *EtcdReconciler) recreateStatefulset(ctx context.Context, ss *appsv1.StatefulSet) error {
	skipDelete := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if !skipDelete {
			if err := r.Delete(ctx, ss); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		skipDelete = true
		return r.Create(ctx, ss)
	})
	return err
}

func (r *EtcdReconciler) getStatefulSetFromEtcd(etcd *druidv1alpha1.Etcd, values map[string]interface{}) (*appsv1.StatefulSet, error) {
	var err error
	decoded := &appsv1.StatefulSet{}
	statefulSetPath := getChartPathForStatefulSet()
	chartPath := getChartPath()
	renderedChart, err := r.chartApplier.Render(chartPath, etcd.Name, etcd.Namespace, values)
	if err != nil {
		return nil, err
	}
	if _, ok := renderedChart.Files()[statefulSetPath]; !ok {
		return nil, fmt.Errorf("missing configmap template file in the charts: %v", statefulSetPath)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(renderedChart.Files()[statefulSetPath])), 1024)
	if err = decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (r *EtcdReconciler) reconcileEtcd(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd) (*corev1.Service, *appsv1.StatefulSet, error) {

	values, err := r.getMapFromEtcd(etcd)
	if err != nil {
		return nil, nil, err
	}

	chartPath := getChartPath()
	renderedChart, err := r.chartApplier.Render(chartPath, etcd.Name, etcd.Namespace, values)
	if err != nil {
		return nil, nil, err
	}
	svc, err := r.reconcileServices(ctx, logger, etcd, renderedChart)
	if err != nil {
		return nil, nil, err
	}
	if svc != nil {
		values["serviceName"] = svc.Name
	}

	cm, err := r.reconcileConfigMaps(ctx, logger, etcd, renderedChart)
	if err != nil {
		return nil, nil, err
	}
	if cm != nil {
		values["configMapName"] = cm.Name
	}

	ss, err := r.reconcileStatefulSet(ctx, logger, etcd, values)
	if err != nil {
		return nil, nil, err
	}

	return svc, ss, nil
}

func checkEtcdOwnerReference(refs []metav1.OwnerReference, etcd *druidv1alpha1.Etcd) bool {
	for _, ownerRef := range refs {
		if ownerRef.UID == etcd.UID {
			return true
		}
	}
	return false
}

func checkEtcdAnnotations(annotations map[string]string, etcd metav1.Object) bool {
	var (
		ownedBy, ownerType string
		ok                 bool
	)
	if annotations == nil {
		return false
	}
	if ownedBy, ok = annotations[common.GardenerOwnedBy]; !ok {
		return ok
	}
	if ownerType, ok = annotations[common.GardenerOwnerType]; !ok {
		return ok
	}
	return ownedBy == fmt.Sprintf("%s/%s", etcd.GetNamespace(), etcd.GetName()) &&
		ownerType == strings.ToLower(etcdGVK.Kind)

}

func (r *EtcdReconciler) getMapFromEtcd(etcd *druidv1alpha1.Etcd) (map[string]interface{}, error) {
	var (
		images map[string]*imagevector.Image
		err    error
	)

	imageNames := []string{
		common.Etcd,
		common.BackupRestore,
	}

	if etcd.Spec.Etcd.Image == nil || etcd.Spec.Backup.Image == nil {

		images, err = imagevector.FindImages(r.ImageVector, imageNames)
		if err != nil {
			return map[string]interface{}{}, err
		}
	}

	var statefulsetReplicas int
	if etcd.Spec.Replicas != 0 {
		statefulsetReplicas = 1
	}

	etcdValues := map[string]interface{}{
		"defragmentationSchedule": etcd.Spec.Etcd.DefragmentationSchedule,
		"enableTLS":               (etcd.Spec.Etcd.TLS != nil),
		"pullPolicy":              corev1.PullIfNotPresent,
		// "username":                etcd.Spec.Etcd.Username,
		// "password":                etcd.Spec.Etcd.Password,
	}

	if etcd.Spec.Etcd.Resources != nil {
		etcdValues["resources"] = etcd.Spec.Etcd.Resources
	}

	if etcd.Spec.Etcd.Metrics != nil {
		etcdValues["metrics"] = etcd.Spec.Etcd.Metrics
	}

	if etcd.Spec.Etcd.ServerPort != nil {
		etcdValues["serverPort"] = etcd.Spec.Etcd.ServerPort
	}

	if etcd.Spec.Etcd.ClientPort != nil {
		etcdValues["clientPort"] = etcd.Spec.Etcd.ClientPort
	}

	if etcd.Spec.Etcd.Image == nil {
		val, ok := images[common.Etcd]
		if !ok {
			return map[string]interface{}{}, fmt.Errorf("either etcd resource or image vector should have %s image", common.Etcd)
		}
		etcdValues["image"] = val.String()
	} else {
		etcdValues["image"] = etcd.Spec.Etcd.Image
	}

	var quota int64 = 8 * 1024 * 1024 * 1024 // 8Gi
	if etcd.Spec.Etcd.Quota != nil {
		quota = etcd.Spec.Etcd.Quota.Value()
	}

	var deltaSnapshotMemoryLimit int64 = 100 * 1024 * 1024 // 100Mi
	if etcd.Spec.Backup.DeltaSnapshotMemoryLimit != nil {
		deltaSnapshotMemoryLimit = etcd.Spec.Backup.DeltaSnapshotMemoryLimit.Value()
	}

	backupValues := map[string]interface{}{
		"pullPolicy":               corev1.PullIfNotPresent,
		"etcdQuotaBytes":           quota,
		"etcdConnectionTimeout":    "5m",
		"snapstoreTempDir":         "/var/etcd/data/temp",
		"deltaSnapshotMemoryLimit": deltaSnapshotMemoryLimit,
	}

	if etcd.Spec.Backup.Resources != nil {
		backupValues["resources"] = etcd.Spec.Backup.Resources
	}

	if etcd.Spec.Backup.FullSnapshotSchedule != nil {
		backupValues["fullSnapshotSchedule"] = etcd.Spec.Backup.FullSnapshotSchedule
	}

	if etcd.Spec.Backup.GarbageCollectionPolicy != nil {
		backupValues["garbageCollectionPolicy"] = etcd.Spec.Backup.GarbageCollectionPolicy
	}

	if etcd.Spec.Backup.GarbageCollectionPeriod != nil {
		backupValues["garbageCollectionPeriod"] = etcd.Spec.Backup.GarbageCollectionPeriod
	}

	if etcd.Spec.Backup.DeltaSnapshotPeriod != nil {
		backupValues["deltaSnapshotPeriod"] = etcd.Spec.Backup.DeltaSnapshotPeriod
	}

	if etcd.Spec.Backup.Port != nil {
		backupValues["port"] = etcd.Spec.Backup.Port
	}

	if etcd.Spec.Backup.SnapshotCompression != nil {
		compressionValues := make(map[string]interface{})
		if etcd.Spec.Backup.SnapshotCompression.Enabled {
			compressionValues["enabled"] = etcd.Spec.Backup.SnapshotCompression.Enabled
		}
		if etcd.Spec.Backup.SnapshotCompression.Policy != nil {
			compressionValues["policy"] = etcd.Spec.Backup.SnapshotCompression.Policy
		}
		backupValues["compression"] = compressionValues
	}

	if etcd.Spec.Backup.Image == nil {
		val, ok := images[common.BackupRestore]
		if !ok {
			return map[string]interface{}{}, fmt.Errorf("either etcd resource or image vector should have %s image", common.BackupRestore)
		}
		backupValues["image"] = val.String()
	} else {
		backupValues["image"] = etcd.Spec.Backup.Image
	}

	volumeClaimTemplateName := etcd.Name
	if etcd.Spec.VolumeClaimTemplate != nil && len(*etcd.Spec.VolumeClaimTemplate) != 0 {
		volumeClaimTemplateName = *etcd.Spec.VolumeClaimTemplate
	}

	sharedConfigValues := map[string]interface{}{
		"autoCompactionMode":      druidv1alpha1.Periodic,
		"autoCompactionRetention": DefaultAutoCompactionRetention,
	}

	if etcd.Spec.Common.AutoCompactionMode != nil {
		sharedConfigValues["autoCompactionMode"] = etcd.Spec.Common.AutoCompactionMode
	}

	if etcd.Spec.Common.AutoCompactionRetention != nil {
		sharedConfigValues["autoCompactionRetention"] = etcd.Spec.Common.AutoCompactionRetention
	}

	values := map[string]interface{}{
		"name":                    etcd.Name,
		"uid":                     etcd.UID,
		"selector":                etcd.Spec.Selector,
		"labels":                  etcd.Spec.Labels,
		"annotations":             etcd.Spec.Annotations,
		"etcd":                    etcdValues,
		"backup":                  backupValues,
		"sharedConfig":            sharedConfigValues,
		"replicas":                etcd.Spec.Replicas,
		"statefulsetReplicas":     statefulsetReplicas,
		"serviceName":             fmt.Sprintf("%s-client", etcd.Name),
		"configMapName":           fmt.Sprintf("etcd-bootstrap-%s", string(etcd.UID[:6])),
		"volumeClaimTemplateName": volumeClaimTemplateName,
	}

	if etcd.Spec.StorageCapacity != nil {
		values["storageCapacity"] = etcd.Spec.StorageCapacity
	}

	if etcd.Spec.StorageClass != nil {
		values["storageClass"] = etcd.Spec.StorageClass
	}

	if etcd.Spec.PriorityClassName != nil {
		values["priorityClassName"] = *etcd.Spec.PriorityClassName
	}

	if etcd.Spec.Etcd.TLS != nil {
		values["tlsServerSecret"] = etcd.Spec.Etcd.TLS.ServerTLSSecretRef.Name
		values["tlsClientSecret"] = etcd.Spec.Etcd.TLS.ClientTLSSecretRef.Name
		values["tlsCASecret"] = etcd.Spec.Etcd.TLS.TLSCASecretRef.Name
	}

	if etcd.Spec.Backup.Store != nil {
		storageProvider, err := utils.StorageProviderFromInfraProvider(etcd.Spec.Backup.Store.Provider)
		if err != nil {
			return nil, err
		}
		storeValues := map[string]interface{}{
			"storePrefix":     etcd.Spec.Backup.Store.Prefix,
			"storageProvider": storageProvider,
		}
		if etcd.Spec.Backup.Store.Container != nil {
			storeValues["storageContainer"] = etcd.Spec.Backup.Store.Container
		}
		if etcd.Spec.Backup.Store.SecretRef != nil {
			storeValues["storeSecret"] = etcd.Spec.Backup.Store.SecretRef.Name
		}

		values["store"] = storeValues
	}

	return values, nil
}

func (r *EtcdReconciler) addFinalizersToDependantSecrets(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd) error {

	secrets := []*corev1.SecretReference{}
	if etcd.Spec.Etcd.TLS != nil {
		// As the secrets inside TLS field are required, we error in case they are not found.
		secrets = append(secrets,
			&etcd.Spec.Etcd.TLS.ClientTLSSecretRef,
			&etcd.Spec.Etcd.TLS.ServerTLSSecretRef,
			&etcd.Spec.Etcd.TLS.TLSCASecretRef,
		)
	}
	if etcd.Spec.Backup.Store != nil && etcd.Spec.Backup.Store.SecretRef != nil {
		// As the store secret is required, we error in case it is not found as well.
		secrets = append(secrets, etcd.Spec.Backup.Store.SecretRef)
	}

	for _, secretRef := range secrets {
		secret := &corev1.Secret{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Name:      secretRef.Name,
			Namespace: etcd.Namespace,
		}, secret); err != nil {
			return err
		}
		if finalizers := sets.NewString(secret.Finalizers...); !finalizers.Has(FinalizerName) {
			logger.Info("Adding finalizer for secret", "secret", kutil.Key(secret.Namespace, secret.Name).String())
			finalizers.Insert(FinalizerName)
			secret.Finalizers = finalizers.UnsortedList()
			if err := r.Update(ctx, secret); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *EtcdReconciler) removeFinalizersToDependantSecrets(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd) error {
	secrets := []*corev1.SecretReference{}
	if etcd.Spec.Etcd.TLS != nil {
		secrets = append(secrets,
			&etcd.Spec.Etcd.TLS.ClientTLSSecretRef,
			&etcd.Spec.Etcd.TLS.ServerTLSSecretRef,
			&etcd.Spec.Etcd.TLS.TLSCASecretRef,
		)
	}
	if etcd.Spec.Backup.Store != nil && etcd.Spec.Backup.Store.SecretRef != nil {
		secrets = append(secrets, etcd.Spec.Backup.Store.SecretRef)
	}

	for _, secretRef := range secrets {
		secret := &corev1.Secret{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Name:      secretRef.Name,
			Namespace: etcd.Namespace,
		}, secret); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
		} else if finalizers := sets.NewString(secret.Finalizers...); finalizers.Has(FinalizerName) {
			logger.Info("Removing finalizer from secret", "secret", kutil.Key(secret.Namespace, secret.Name).String())
			finalizers.Delete(FinalizerName)
			secret.Finalizers = finalizers.UnsortedList()
			if err := r.Update(ctx, secret); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

func (r *EtcdReconciler) removeDependantStatefulset(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd) error {
	selector, err := metav1.LabelSelectorAsSelector(etcd.Spec.Selector)
	if err != nil {
		return err
	}

	statefulSets := &appsv1.StatefulSetList{}
	if err = r.List(ctx, statefulSets, client.InNamespace(etcd.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return err
	}
	for _, sts := range statefulSets.Items {
		if canDeleteStatefulset(&sts, etcd) {
			logger.Info("Deleting statefulset", "statefulset", kutil.Key(sts.GetNamespace(), sts.GetName()).String())
			if err := r.Delete(ctx, &sts); err != nil {
				return err
			}
		}
	}
	return nil
}

func canDeleteStatefulset(sts *appsv1.StatefulSet, etcd *druidv1alpha1.Etcd) bool {
	// Adding check for ownerReference to have the same delete path for statefulset.
	// The statefulset with ownerReference will be deleted automatically when etcd is
	// delete but we would like to explicitly delete it to maintain uniformity in the
	// delete path.
	return checkEtcdOwnerReference(sts.GetOwnerReferences(), etcd) ||
		checkEtcdAnnotations(sts.GetAnnotations(), etcd)

}

func (r *EtcdReconciler) updateEtcdErrorStatus(ctx context.Context, etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet, lastError error) error {
	err := kutil.TryUpdateStatus(ctx, retry.DefaultBackoff, r.Client, etcd, func() error {
		lastErrStr := fmt.Sprintf("%v", lastError)
		etcd.Status.LastError = &lastErrStr
		etcd.Status.ObservedGeneration = &etcd.Generation
		if sts != nil {
			ready := CheckStatefulSet(etcd, sts) == nil
			etcd.Status.Ready = &ready
		}
		return nil
	})

	if err != nil {
		return err
	}
	return r.removeOperationAnnotation(ctx, etcd)
}

func (r *EtcdReconciler) updateEtcdStatus(ctx context.Context, etcd *druidv1alpha1.Etcd, svc *corev1.Service, sts *appsv1.StatefulSet) error {
	err := kutil.TryUpdateStatus(ctx, retry.DefaultBackoff, r.Client, etcd, func() error {
		ready := CheckStatefulSet(etcd, sts) == nil
		etcd.Status.Ready = &ready
		svcName := svc.Name
		etcd.Status.ServiceName = &svcName
		etcd.Status.LastError = nil
		etcd.Status.ObservedGeneration = &etcd.Generation
		return nil
	})

	if err != nil {
		return err
	}
	return r.removeOperationAnnotation(ctx, etcd)
}

func (r *EtcdReconciler) waitUntilStatefulSetReady(ctx context.Context, logger logr.Logger, etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet) (*appsv1.StatefulSet, error) {
	var (
		ss = &appsv1.StatefulSet{}
	)

	err := gardenerretry.UntilTimeout(ctx, DefaultInterval, DefaultTimeout, func(ctx context.Context) (bool, error) {
		if err := r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, ss); err != nil {
			if apierrors.IsNotFound(err) {
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}
		if err := CheckStatefulSet(etcd, ss); err != nil {
			return gardenerretry.MinorError(err)
		}
		return gardenerretry.Ok()
	})
	if err != nil {
		messages, err2 := r.fetchPVCEventsFor(ctx, ss)
		if err2 != nil {
			logger.Error(err2, "Error while fetching events for depending PVC")
			// don't expose this error since fetching events is a best effort
			// and shouldn't be confused with the actual error
			return ss, err
		}
		if messages != "" {
			return ss, fmt.Errorf("%w\n\n%s", err, messages)
		}
	}

	return ss, err
}

func (r *EtcdReconciler) fetchPVCEventsFor(ctx context.Context, ss *appsv1.StatefulSet) (string, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(ss.GetNamespace())); err != nil {
		return "", err
	}

	var (
		pvcMessages  string
		volumeClaims = ss.Spec.VolumeClaimTemplates
	)
	for _, volumeClaim := range volumeClaims {
		for _, pvc := range pvcs.Items {
			if !strings.HasPrefix(pvc.GetName(), fmt.Sprintf("%s-%s", volumeClaim.Name, ss.Name)) || pvc.Status.Phase == corev1.ClaimBound {
				continue
			}
			messages, err := kutil.FetchEventMessages(ctx, r.Client.Scheme(), r.Client, &pvc, corev1.EventTypeWarning, 2)
			if err != nil {
				return "", err
			}
			if messages != "" {
				pvcMessages += fmt.Sprintf("Warning for PVC %s:\n%s\n", pvc.Name, messages)
			}
		}
	}
	return pvcMessages, nil
}

func (r *EtcdReconciler) removeOperationAnnotation(ctx context.Context, etcd *druidv1alpha1.Etcd) error {
	if _, ok := etcd.Annotations[v1beta1constants.GardenerOperation]; ok {
		delete(etcd.Annotations, v1beta1constants.GardenerOperation)
		return r.Update(ctx, etcd)
	}
	return nil
}

func (r *EtcdReconciler) updateEtcdStatusAsNotReady(ctx context.Context, etcd *druidv1alpha1.Etcd) (*druidv1alpha1.Etcd, error) {
	err := kutil.TryUpdateStatus(ctx, retry.DefaultBackoff, r.Client, etcd, func() error {
		etcd.Status.Ready = nil
		etcd.Status.ReadyReplicas = 0
		return nil
	})
	return etcd, err
}

func (r *EtcdReconciler) claimServices(ctx context.Context, etcd *druidv1alpha1.Etcd, selector labels.Selector, ss *corev1.ServiceList) ([]*corev1.Service, error) {
	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing Machines (see #42639).
	canAdoptFunc := RecheckDeletionTimestamp(func() (metav1.Object, error) {
		foundEtcd := &druidv1alpha1.Etcd{}
		err := r.Get(ctx, types.NamespacedName{Name: etcd.Name, Namespace: etcd.Namespace}, foundEtcd)
		if err != nil {
			return nil, err
		}
		if foundEtcd.UID != etcd.UID {
			return nil, fmt.Errorf("original %v/%v hvpa gone: got uid %v, wanted %v", etcd.Namespace, etcd.Name, foundEtcd.UID, etcd.UID)
		}
		return foundEtcd, nil
	})
	cm := NewEtcdDruidRefManager(r.Client, r.Scheme, etcd, selector, etcdGVK, canAdoptFunc)
	return cm.ClaimServices(ctx, ss)
}

func (r *EtcdReconciler) claimConfigMaps(ctx context.Context, etcd *druidv1alpha1.Etcd, selector labels.Selector, configMaps *corev1.ConfigMapList) ([]*corev1.ConfigMap, error) {
	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing Machines (see #42639).
	canAdoptFunc := RecheckDeletionTimestamp(func() (metav1.Object, error) {
		foundEtcd := &druidv1alpha1.Etcd{}
		err := r.Get(ctx, types.NamespacedName{Name: etcd.Name, Namespace: etcd.Namespace}, foundEtcd)
		if err != nil {
			return nil, err
		}
		if foundEtcd.UID != etcd.UID {
			return nil, fmt.Errorf("original %v/%v hvpa gone: got uid %v, wanted %v", etcd.Namespace, etcd.Name, foundEtcd.UID, etcd.UID)
		}
		return foundEtcd, nil
	})
	cm := NewEtcdDruidRefManager(r.Client, r.Scheme, etcd, selector, etcdGVK, canAdoptFunc)
	return cm.ClaimConfigMaps(ctx, configMaps)
}

// SetupWithManager sets up manager with a new controller and r as the reconcile.Reconciler
func (r *EtcdReconciler) SetupWithManager(mgr ctrl.Manager, workers int, ignoreOperationAnnotation bool) error {
	predicates := []predicate.Predicate{
		druidpredicates.GenerationChangedPredicate{},
		druidpredicates.LastOperationNotSuccessful(),
	}
	builder := ctrl.NewControllerManagedBy(mgr).WithOptions(controller.Options{
		MaxConcurrentReconciles: workers,
	})
	if !ignoreOperationAnnotation {
		predicates = append(predicates, druidpredicates.HasOperationAnnotation())
	}
	builder = builder.WithEventFilter(predicate.Or(predicates...)).For(&druidv1alpha1.Etcd{})
	if ignoreOperationAnnotation {
		builder = builder.Owns(&v1.Service{}).
			Owns(&v1.ConfigMap{}).
			Owns(&appsv1.StatefulSet{})
	}
	return builder.Complete(r)
}
