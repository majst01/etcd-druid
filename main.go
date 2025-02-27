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

package main

import (
	"flag"
	"os"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/etcd-druid/controllers"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	schemev1 "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(schemev1.AddToScheme(scheme))
	utilruntime.Must(druidv1alpha1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr                string
		enableLeaderElection       bool
		leaderElectionID           string
		leaderElectionResourceLock string
		etcdWorkers                int
		custodianWorkers           int
		ignoreOperationAnnotation  bool

		// TODO: migrate default to `leases` in one of the next releases
		defaultLeaderElectionResourceLock = resourcelock.ConfigMapsLeasesResourceLock
		defaultLeaderElectionID           = "druid-leader-election"
	)

	flag.IntVar(&etcdWorkers, "workers", 3, "Number of worker threads of the etcd controller.")
	flag.IntVar(&custodianWorkers, "custodian-workers", 3, "Number of worker threads of the custodian controller.")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", defaultLeaderElectionID, "Name of the resource that leader election will use for holding the leader lock. "+
		"Defaults to 'druid-leader-election'.")
	flag.StringVar(&leaderElectionResourceLock, "leader-election-resource-lock", defaultLeaderElectionResourceLock, "Which resource type to use for leader election. "+
		"Supported options are 'endpoints', 'configmaps', 'leases', 'endpointsleases' and 'configmapsleases'.")
	flag.BoolVar(&ignoreOperationAnnotation, "ignore-operation-annotation", true, "Ignore the operation annotation or not.")

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx := ctrl.SetupSignalHandler()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		ClientDisableCacheFor:      controllers.UncachedObjectList,
		Scheme:                     scheme,
		MetricsBindAddress:         metricsAddr,
		LeaderElection:             enableLeaderElection,
		LeaderElectionID:           leaderElectionID,
		LeaderElectionResourceLock: leaderElectionResourceLock,
	})
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	etcd, err := controllers.NewEtcdReconcilerWithImageVector(mgr)
	if err != nil {
		setupLog.Error(err, "Unable to initialize controller with image vector")
		os.Exit(1)
	}

	if err := etcd.SetupWithManager(mgr, etcdWorkers, ignoreOperationAnnotation); err != nil {
		setupLog.Error(err, "Unable to create controller", "Controller", "Etcd")
		os.Exit(1)
	}

	custodian := controllers.NewEtcdCustodian(mgr)

	if err := custodian.SetupWithManager(ctx, mgr, custodianWorkers); err != nil {
		setupLog.Error(err, "Unable to create controller", "Controller", "Etcd Custodian")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
