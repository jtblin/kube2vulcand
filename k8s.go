package main

import (
	"fmt"
	"net/url"
	"os"

	"github.com/golang/glog"
	kapi "k8s.io/kubernetes/pkg/api"
	kextensions "k8s.io/kubernetes/pkg/apis/extensions"
	kcache "k8s.io/kubernetes/pkg/client/cache"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kclientcmd "k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	kframework "k8s.io/kubernetes/pkg/controller/framework"
	kSelector "k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/util"
)

// TODO: evaluate using pkg/client/clientcmd
func newKubeClient() (*kclient.Client, error) {
	var (
		config    *kclient.Config
		err       error
		masterURL string
	)
	// If the user specified --kube_master_url, expand env vars and verify it.
	if *argKubeMasterURL != "" {
		masterURL, err = expandKubeMasterURL()
		if err != nil {
			return nil, err
		}
	}

	if masterURL != "" && *argKubecfgFile == "" {
		// Only --kube_master_url was provided.
		config = &kclient.Config{Host: masterURL}
	} else {
		// We either have:
		//  1) --kube_master_url and --kubecfg_file
		//  2) just --kubecfg_file
		//  3) neither flag
		// In any case, the logic is the same.  If (3), this will automatically
		// fall back on the service account token.
		overrides := &kclientcmd.ConfigOverrides{}
		overrides.ClusterInfo.Server = masterURL                                     // might be "", but that is OK
		rules := &kclientcmd.ClientConfigLoadingRules{ExplicitPath: *argKubecfgFile} // might be "", but that is OK
		if config, err = kclientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig(); err != nil {
			return nil, err
		}
	}

	config.Version = k8sAPIVersion
	glog.Infof("Using %s for kubernetes master", config.Host)
	glog.Infof("Using kubernetes API %s", config.Version)
	return kclient.New(config)
}

func expandKubeMasterURL() (string, error) {
	parsedURL, err := url.Parse(os.ExpandEnv(*argKubeMasterURL))
	if err != nil {
		return "", fmt.Errorf("failed to parse --kube_master_url %s - %v", *argKubeMasterURL, err)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" || parsedURL.Host == ":" {
		return "", fmt.Errorf("invalid --kube_master_url specified %s", *argKubeMasterURL)
	}
	return parsedURL.String(), nil
}

func watchForIngresses(kubeClient *kclient.ExtensionsClient, kv *kube2vulcand) kcache.Store {
	ingressStore, ingressController := kframework.NewInformer(
		createIngressLW(kubeClient),
		&kextensions.Ingress{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc:    kv.newIngress,
			DeleteFunc: kv.removeIngress,
			UpdateFunc: kv.updateIngress,
		},
	)
	go ingressController.Run(util.NeverStop)
	return ingressStore
}

// Returns a cache.ListWatch that gets all changes to ingresses.
func createIngressLW(kubeClient *kclient.ExtensionsClient) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "ingresses", kapi.NamespaceAll, kSelector.Everything())
}
