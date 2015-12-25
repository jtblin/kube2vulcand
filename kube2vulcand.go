/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

// kube2vulcand is a bridge between Kubernetes and vulcand.  It watches the
// Kubernetes master for changes in Services and Ingresses and manifests them
// into etcd for vulcand to load balance as backends and frontends.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"strconv"

	etcd "github.com/coreos/go-etcd/etcd"
	"github.com/golang/glog"
	"github.com/spf13/pflag"
	kapi "k8s.io/kubernetes/pkg/api"
	kextensions "k8s.io/kubernetes/pkg/apis/extensions"
	kcache "k8s.io/kubernetes/pkg/client/cache"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kclientcmd "k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	kframework "k8s.io/kubernetes/pkg/controller/framework"
	kSelector "k8s.io/kubernetes/pkg/fields"
	etcdstorage "k8s.io/kubernetes/pkg/storage/etcd"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/wait"
)

var (
	argEtcdMutationTimeout = pflag.Duration("etcd-mutation-timeout", 10*time.Second, "Crash after retrying etcd mutation for a specified duration")
	argEtcdServer          = pflag.String("etcd-server", "http://127.0.0.1:4001", "URL to etcd server")
	argKubecfgFile         = pflag.String("kubecfg-file", "", "Location of kubecfg file for access to kubernetes master service; --kube-master-url overrides the URL part of this; if neither this nor --kube-master-url are provided, defaults to service account tokens")
	argKubeMasterURL       = pflag.String("kube-master-url", "", "URL to reach kubernetes master. Env variables in this flag will be expanded.")
)

const (
	// vulcand backend type
	backendType = "http"
	// Base vulcand etcd key
	etcdKey = "/vulcand"
	// Maximum number of attempts to connect to etcd server.
	k8sAPIVersion      = "v1"
	maxConnectAttempts = 12
	// Resync period for the kube controller loop.
	resyncPeriod = 30 * time.Minute
)

type etcdClient interface {
	Set(path, value string, ttl uint64) (*etcd.Response, error)
	RawGet(key string, sort, recursive bool) (*etcd.RawResponse, error)
	Delete(path string, recursive bool) (*etcd.Response, error)
}

type kube2vulcand struct {
	// Etcd client.
	etcdClient etcdClient
	// Etcd mutation timeout.
	etcdMutationTimeout time.Duration
	// A cache that contains all the endpoints in the system.
	ingressesStore kcache.Store
	// A cache that contains all the servicess in the system.
	servicesStore kcache.Store
}

// Removes 'backend' from etcd.
func (kv *kube2vulcand) removeBackend(name string) error {
	glog.V(1).Infof("Removing %s backend from vulcand", name)
	resp, err := kv.etcdClient.RawGet(backendPath(name), false, true)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		glog.V(1).Infof("Backend %q does not exist in etcd", name)
		return nil
	}
	_, err = kv.etcdClient.Delete(backendPath(name), true)
	return err
}

// Removes 'frontend' from etcd.
func (kv *kube2vulcand) removeFrontend(name string) error {
	glog.V(1).Infof("Removing frontend %s from vulcand", name)
	resp, err := kv.etcdClient.RawGet(frontendPath(name), false, true)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		glog.V(1).Infof("Frontend %q does not exist in etcd", name)
		return nil
	}
	_, err = kv.etcdClient.Delete(frontendPath(name), true)
	return err
}

func backendPath(name string) string {
	return vulcandPath("backends", name, "backend")
}

func backendServerPath(name string) string {
	return vulcandPath("backends", name, "servers", "server")
}

func frontendPath(name string) string {
	return vulcandPath("frontends", name, "frontend")
}

func vulcandPath(keys ...string) string {
	return strings.Join(append([]string{etcdKey}, keys...), "/")
}

func (kv *kube2vulcand) writeVulcandBackend(name string, data string) error {
	// Set with no TTL, and hope that kubernetes events are accurate.
	_, err := kv.etcdClient.Set(backendPath(name), data, uint64(0))
	return err
}

func (kv *kube2vulcand) writeVulcandBackendServer(name string, data string) error {
	// Set with no TTL, and hope that kubernetes events are accurate.
	_, err := kv.etcdClient.Set(backendServerPath(name), data, uint64(0))
	return err
}

func (kv *kube2vulcand) writeVulcandFrontend(name string, data string) error {
	// Set with no TTL, and hope that kubernetes events are accurate.
	_, err := kv.etcdClient.Set(frontendPath(name), data, uint64(0))
	return err
}

func (kv *kube2vulcand) addBackend(name string, backend *Backend) error {
	data, err := json.Marshal(backend)
	if err != nil {
		return err
	}
	glog.V(1).Infof("Setting backend: %s -> %v", name, backend)
	if err = kv.writeVulcandBackend(name, string(data)); err != nil {
		return err
	}
	return nil
}

func (kv *kube2vulcand) addBackendServer(name string, server *BackendServer) error {
	data, err := json.Marshal(server)
	if err != nil {
		return err
	}
	glog.V(1).Infof("Setting backend server: %s -> %v", name, server)
	if err = kv.writeVulcandBackendServer(name, string(data)); err != nil {
		return err
	}
	return nil
}

func (kv *kube2vulcand) addFrontend(name string, frontend *Frontend) error {
	data, err := json.Marshal(frontend)
	if err != nil {
		return err
	}
	glog.V(1).Infof("Setting frontend: %s -> %v", name, frontend)
	if err = kv.writeVulcandFrontend(name, string(data)); err != nil {
		return err
	}
	return nil
}

// Implements retry logic for arbitrary mutator. Crashes after retrying for
// etcd_mutation_timeout.
func (kv *kube2vulcand) mutateEtcdOrDie(mutator func() error) {
	timeout := time.After(kv.etcdMutationTimeout)
	for {
		select {
		case <-timeout:
			glog.Fatalf("Failed to mutate etcd for %v using mutator: %v", kv.etcdMutationTimeout, mutator)
		default:
			if err := mutator(); err != nil {
				delay := 50 * time.Millisecond
				glog.V(1).Infof("Failed to mutate etcd using mutator: %v due to: %v. Will retry in: %v", mutator, err, delay)
				time.Sleep(delay)
			} else {
				return
			}
		}
	}
}

func buildBackendIDString(namespace, name, port string) string {
	return fmt.Sprintf("%s:%s:%s", namespace, name, port)
}

func buildBackendServerString(namespace, name, port string) string {
	return fmt.Sprintf("http://%s.%s:%s", name, namespace, port)
}

func buildFrontendNameString(labels ...string) string {
	return strings.Join(labels, "-")
}

func buildRouteString(host, path string) string {
	return fmt.Sprintf("Host(`%s`) && Path(`%s`)", host, path)
}

// Returns a cache.ListWatch that gets all changes to ingresses.
func createIngressLW(kubeClient *kclient.ExtensionsClient) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "ingresses", kapi.NamespaceAll, kSelector.Everything())
}

// Returns a cache.ListWatch that gets all changes to services.
func createServiceLW(kubeClient *kclient.Client) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "services", kapi.NamespaceAll, kSelector.Everything())
}

type Backend struct {
	Type string
}

type BackendServer struct {
	URL string
}

type Frontend struct {
	BackendID string `json:"BackendId"`
	Route     string
	Type      string
}

func (kv *kube2vulcand) newIngress(obj interface{}) {
	if ing, ok := obj.(*kextensions.Ingress); ok {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				frontendName := buildFrontendNameString(ing.Name, ing.Namespace, rule.Host, path.Path)
				backendID := buildBackendIDString(ing.Namespace, path.Backend.ServiceName, strconv.Itoa(path.Backend.ServicePort.IntVal))
				frontend := &Frontend{BackendID: backendID, Route: buildRouteString(rule.Host, path.Path)}
				kv.mutateEtcdOrDie(func() error { return kv.addFrontend(frontendName, frontend) })
			}
		}
	}
}

func (kv *kube2vulcand) removeIngress(obj interface{}) {
	if ing, ok := obj.(*kextensions.Ingress); ok {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				name := buildFrontendNameString(ing.Name, ing.Namespace, rule.Host, path.Path)
				kv.mutateEtcdOrDie(func() error { return kv.removeFrontend(name) })
			}
		}
	}
}

func (kv *kube2vulcand) updateIngress(oldObj, newObj interface{}) {
	// TODO: Avoid unwanted updates.
	kv.removeIngress(oldObj)
	kv.newIngress(newObj)
}

func (kv *kube2vulcand) newService(obj interface{}) {
	if s, ok := obj.(*kapi.Service); ok {
		for _, port := range s.Spec.Ports {
			backendID := buildBackendIDString(s.Namespace, s.Name, strconv.Itoa(port.Port))
			kv.mutateEtcdOrDie(func() error { return kv.addBackend(backendID, &Backend{Type: backendType}) })
			backendServer := buildBackendServerString(s.Namespace, s.Name, strconv.Itoa(port.Port))
			kv.mutateEtcdOrDie(func() error { return kv.addBackendServer(backendID, &BackendServer{URL: backendServer}) })
		}
	}
}

func (kv *kube2vulcand) removeService(obj interface{}) {
	if s, ok := obj.(*kapi.Service); ok {
		for _, port := range s.Spec.Ports {
			backendID := buildBackendIDString(s.Namespace, s.Name, strconv.Itoa(port.Port))
			kv.mutateEtcdOrDie(func() error { return kv.removeBackend(backendID) })
		}
	}
}

func (kv *kube2vulcand) updateService(oldObj, newObj interface{}) {
	// TODO: Avoid unwanted updates.
	kv.removeService(oldObj)
	kv.newService(newObj)
}

func newEtcdClient(etcdServer string) (*etcd.Client, error) {
	var (
		client *etcd.Client
		err    error
	)
	for attempt := 1; attempt <= maxConnectAttempts; attempt++ {
		if _, err = etcdstorage.GetEtcdVersion(etcdServer); err == nil {
			break
		}
		if attempt == maxConnectAttempts {
			break
		}
		glog.Infof("[Attempt: %d] Attempting access to etcd after 5 second sleep", attempt)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to etcd server: %v, error: %v", etcdServer, err)
	}
	glog.Infof("Etcd server found: %v", etcdServer)

	// loop until we have > 0 machines && machines[0] != ""
	poll, timeout := 1*time.Second, 10*time.Second
	if err := wait.Poll(poll, timeout, func() (bool, error) {
		if client = etcd.NewClient([]string{etcdServer}); client == nil {
			return false, fmt.Errorf("etcd.NewClient returned nil")
		}
		client.SyncCluster()
		machines := client.GetCluster()
		if len(machines) == 0 || len(machines[0]) == 0 {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("Timed out after %s waiting for at least 1 synchronized etcd server in the cluster. Error: %v", timeout, err)
	}
	return client, nil
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

func watchForServices(kubeClient *kclient.Client, kv *kube2vulcand) kcache.Store {
	serviceStore, serviceController := kframework.NewInformer(
		createServiceLW(kubeClient),
		&kapi.Service{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc:    kv.newService,
			DeleteFunc: kv.removeService,
			UpdateFunc: kv.updateService,
		},
	)
	go serviceController.Run(util.NeverStop)
	return serviceStore
}

func main() {
	// TODO: replace by non k8s code / logs
	util.InitFlags()
	util.InitLogs()
	defer util.FlushLogs()

	kv := kube2vulcand{etcdMutationTimeout: *argEtcdMutationTimeout}

	var err error
	// TODO: Validate input flags.
	if kv.etcdClient, err = newEtcdClient(*argEtcdServer); err != nil {
		glog.Fatalf("Failed to create etcd client - %v", err)
	}

	kubeClient, err := newKubeClient()
	if err != nil {
		glog.Fatalf("Failed to create a kubernetes client: %v", err)
	}

	kv.ingressesStore = watchForIngresses(kubeClient.ExtensionsClient, &kv)
	kv.servicesStore = watchForServices(kubeClient, &kv)

	select {}
}
