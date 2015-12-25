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

// inspiration: https://github.com/kubernetes/kubernetes/blob/master/cluster/addons/dns/kube2sky/kube2sky.go

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
	"strconv"
	"strings"
	"time"

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
	argVersion             = pflag.Bool("version", false, "Display the version number and exit.")
)

// VERSION will be set at build time via linker magic.
var VERSION string

const (
	// vulcand backend supported type
	backendType = "http"
	// Base vulcand etcd key
	etcdKey = "/vulcand"
	// vulcand frontend supported type
	frontendType = "http"
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
	// A cache that contains all the ingresses in the system.
	ingressesStore kcache.Store
}

type vulcandEndpoint interface {
	basePath(name string) string
	endpointType() string
	path(name string) string
}

type backend struct {
	Type string
}

func (b *backend) endpointType() string {
	return "backend"
}

func (b *backend) basePath(name string) string {
	return vulcandPath("backends", name)
}

func (b *backend) path(name string) string {
	return vulcandPath("backends", name, "backend")
}

type frontend struct {
	BackendID string `json:"BackendId"`
	Route     string
	Type      string
}

func (f *frontend) endpointType() string {
	return "frontend"
}

func (f *frontend) basePath(name string) string {
	return vulcandPath("frontends", name)
}

func (f *frontend) path(name string) string {
	return vulcandPath("frontends", name, "frontend")
}

type server struct {
	URL string
}

func (bs *server) endpointType() string {
	return "backend server"
}

func (bs *server) basePath(name string) string {
	return vulcandPath("backends", name)
}

func (bs *server) path(name string) string {
	return vulcandPath("backends", name, "servers", "server")
}

func vulcandPath(keys ...string) string {
	return strings.Join(append([]string{etcdKey}, keys...), "/")
}

func (kv *kube2vulcand) writeVulcandEndpoint(name string, data string) error {
	// Set with no TTL, and hope that kubernetes events are accurate.
	_, err := kv.etcdClient.Set(name, data, uint64(0))
	return err
}

// Add 'endpoint' to etcd e.g. frontend, backend, backend server.
func (kv *kube2vulcand) addEndpoint(name string, endpoint vulcandEndpoint) error {
	data, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	glog.V(1).Infof("Setting %s: %s -> %v", endpoint.endpointType(), name, endpoint)
	if err = kv.writeVulcandEndpoint(endpoint.path(name), string(data)); err != nil {
		return err
	}
	return nil
}

// Removes 'endpoint' from etcd e.g. frontend, backend, backend server.
func (kv *kube2vulcand) removeEndpoint(name string, endpoint vulcandEndpoint) error {
	glog.V(1).Infof("Removing %s %s from vulcand", endpoint.endpointType(), name)
	resp, err := kv.etcdClient.RawGet(endpoint.basePath(name), false, true)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		glog.V(1).Infof("%s %q does not exist in etcd", endpoint.endpointType(), name)
		return nil
	}
	_, err = kv.etcdClient.Delete(endpoint.basePath(name), true)
	return err
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
				glog.V(1).Infof("Failed to mutate etcd using mutator: %v due to: %v. Will retry in: %v",
					mutator, err, delay)
				time.Sleep(delay)
			} else {
				return
			}
		}
	}
}

func buildBackendIDString(protocol, name, namespace, port string) string {
	return fmt.Sprintf("%s:%s:%s:%s", protocol, name, namespace, port)
}

func buildBackendServerURLString(name, namespace, port string) string {
	return fmt.Sprintf("http://%s.%s:%s", name, namespace, port)
}

func buildFrontendNameString(protocol, name, namespace, host, path string) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s", protocol, name, namespace, host, path)
}

func buildRouteString(host, path string) string {
	return fmt.Sprintf("Host(`%s`) && Path(`%s`)", host, path)
}

// Returns a cache.ListWatch that gets all changes to ingresses.
func createIngressLW(kubeClient *kclient.ExtensionsClient) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "ingresses", kapi.NamespaceAll, kSelector.Everything())
}

func (kv *kube2vulcand) newIngress(obj interface{}) {
	if ing, ok := obj.(*kextensions.Ingress); ok {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				port := strconv.Itoa(path.Backend.ServicePort.IntVal)
				frontendID := buildFrontendNameString(
					frontendType, ing.Name, ing.Namespace, rule.Host, url.QueryEscape(path.Path),
				)
				backendID := buildBackendIDString(backendType, path.Backend.ServiceName, ing.Namespace, port)
				serverURL := buildBackendServerURLString(path.Backend.ServiceName, ing.Namespace, port)
				kv.mutateEtcdOrDie(func() error {
					if err := kv.addEndpoint(backendID, &backend{Type: backendType}); err != nil {
						return err
					}
					if err := kv.addEndpoint(backendID, &server{URL: serverURL}); err != nil {
						return err
					}
					return kv.addEndpoint(frontendID, &frontend{
						BackendID: backendID, Route: buildRouteString(rule.Host, path.Path), Type: frontendType,
					})
				})
			}
		}
	}
}

func (kv *kube2vulcand) removeIngress(obj interface{}) {
	if ing, ok := obj.(*kextensions.Ingress); ok {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				frontendID := buildFrontendNameString(frontendType, ing.Name, ing.Namespace, rule.Host,
					url.QueryEscape(path.Path))
				backendID := buildBackendIDString(backendType, path.Backend.ServiceName, ing.Namespace,
					strconv.Itoa(path.Backend.ServicePort.IntVal))
				kv.mutateEtcdOrDie(func() error {
					if err := kv.removeEndpoint(frontendID, new(frontend)); err != nil {
						return err
					}
					return kv.removeEndpoint(backendID, new(backend))
				})
			}
		}
	}
}

func (kv *kube2vulcand) updateIngress(oldObj, newObj interface{}) {
	// TODO: Avoid unwanted updates.
	kv.removeIngress(oldObj)
	kv.newIngress(newObj)
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

func main() {
	// TODO: replace by non k8s code / logs
	util.InitFlags()
	util.InitLogs()
	defer util.FlushLogs()

	if *argVersion {
		fmt.Println(VERSION)
		os.Exit(0)
	}

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

	select {}
}
