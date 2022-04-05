/*
Copyright 2021 The KCP Authors.

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

package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	crdexternalversions "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	kcpclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	kcpexternalversions "github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
	"github.com/kcp-dev/kcp/pkg/reconciler/apiresource"
	"github.com/kcp-dev/kcp/pkg/reconciler/workload/apiimporter"
	clusterapiimporter "github.com/kcp-dev/kcp/pkg/reconciler/workload/apiimporter"
	"github.com/kcp-dev/kcp/pkg/reconciler/workload/syncer"
)

const resyncPeriod = 10 * time.Hour

func bindOptions(fs *pflag.FlagSet) *options {
	o := options{
		ApiImporterOptions: apiimporter.BindOptions(apiimporter.DefaultOptions(), fs),
		ApiResourceOptions: apiresource.BindOptions(apiresource.DefaultOptions(), fs),
		SyncerOptions:      syncer.BindOptions(syncer.DefaultOptions(), fs),
	}
	fs.StringVar(&o.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig")
	return &o
}

type options struct {
	// in the all-in-one startup, client credentials already exist; in this
	// standalone startup, we need to load credentials ourselves
	kubeconfigPath string

	ApiImporterOptions *apiimporter.Options
	ApiResourceOptions *apiresource.Options
	SyncerOptions      *syncer.Options
}

func (o *options) Validate() error {
	if o.kubeconfigPath == "" {
		return errors.New("--kubeconfig is required")
	}
	if err := o.ApiImporterOptions.Validate(); err != nil {
		return err
	}
	if err := o.ApiResourceOptions.Validate(); err != nil {
		return err
	}

	return o.SyncerOptions.Validate()
}

func main() {
	// Setup signal handler for a cleaner shutdown
	ctx := genericapiserver.SetupSignalContext()

	fs := pflag.NewFlagSet("cluster-controller", pflag.ContinueOnError)
	options := bindOptions(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := options.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	configLoader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: options.kubeconfigPath},
		&clientcmd.ConfigOverrides{})

	config, err := configLoader.ClientConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	kubeconfig, err := configLoader.RawConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	crdClusterClient, err := apiextensionsclient.NewClusterForConfig(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	kcpClusterClient, err := kcpclient.NewClusterForConfig(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	kcpSharedInformerFactory := kcpexternalversions.NewSharedInformerFactoryWithOptions(kcpclient.NewForConfigOrDie(config), resyncPeriod)
	crdSharedInformerFactory := crdexternalversions.NewSharedInformerFactoryWithOptions(apiextensionsclient.NewForConfigOrDie(config), resyncPeriod)

	apiImporter, err := clusterapiimporter.NewController(
		kcpClusterClient,
		kcpSharedInformerFactory.Workload().V1alpha1().WorkloadClusters(),
		kcpSharedInformerFactory.Apiresource().V1alpha1().APIResourceImports(),
		options.ApiImporterOptions.ResourcesToSync,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	apiResource, err := apiresource.NewController(
		crdClusterClient,
		kcpClusterClient,
		options.ApiResourceOptions.AutoPublishAPIs,
		kcpSharedInformerFactory.Apiresource().V1alpha1().NegotiatedAPIResources(),
		kcpSharedInformerFactory.Apiresource().V1alpha1().APIResourceImports(),
		crdSharedInformerFactory.Apiextensions().V1().CustomResourceDefinitions(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var optionalSyncer *syncer.Controller
	if manager := options.SyncerOptions.CreateSyncerManager(""); manager == nil {
		klog.Info("syncer not enabled. To enable, supply --pull-mode or --push-mode")
	} else {
		optionalSyncer, err = syncer.NewController(
			crdClusterClient,
			kcpClusterClient,
			kcpSharedInformerFactory.Workload().V1alpha1().WorkloadClusters(),
			kcpSharedInformerFactory.Apiresource().V1alpha1().APIResourceImports(),
			&kubeconfig, // TODO: remove this once the syncer is started externally
			options.SyncerOptions.ResourcesToSync,
			manager,
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	kcpSharedInformerFactory.Start(ctx.Done())
	crdSharedInformerFactory.Start(ctx.Done())

	kcpSharedInformerFactory.WaitForCacheSync(ctx.Done())
	crdSharedInformerFactory.WaitForCacheSync(ctx.Done())

	go apiImporter.Start(ctx)
	go apiResource.Start(ctx, options.ApiResourceOptions.NumThreads)
	if optionalSyncer != nil {
		go optionalSyncer.Start(ctx)
	}

	<-ctx.Done()
}
