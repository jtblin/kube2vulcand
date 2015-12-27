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
	"hash/fnv"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/pflag"
	kextensions "k8s.io/kubernetes/pkg/apis/extensions"
	kcache "k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/util"
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

type kube2vulcand struct {
	// Etcd client.
	etcdClient etcdClient
	// Etcd mutation timeout.
	etcdMutationTimeout time.Duration
	// A cache that contains all the ingresses in the system.
	ingressesStore kcache.Store
}

func (kv *kube2vulcand) writeVulcandEndpoint(ID string, data string) error {
	// Set with no TTL, and hope that kubernetes events are accurate.
	_, err := kv.etcdClient.Set(ID, data, uint64(0))
	return err
}

// Add 'engine' to etcd e.g. frontend, backend, backend server.
func (kv *kube2vulcand) addEngine(ID string, engine vulcandEngine) error {
	data, err := json.Marshal(engine)
	if err != nil {
		return err
	}
	glog.V(1).Infof("Setting %s: %s -> %v", engine.engineType(), ID, engine)
	if err = kv.writeVulcandEndpoint(engine.path(ID), string(data)); err != nil {
		return err
	}
	return nil
}

// Removes 'engine' from etcd e.g. frontend, backend, backend server.
func (kv *kube2vulcand) removeEngine(ID string, engine vulcandEngine) error {
	glog.V(1).Infof("Removing %s %s from vulcand", engine.engineType(), ID)
	resp, err := kv.etcdClient.RawGet(engine.basePath(ID), false, true)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		glog.V(1).Infof("%s %q does not exist in etcd", engine.engineType(), ID)
		return nil
	}
	_, err = kv.etcdClient.Delete(engine.basePath(ID), true)
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

func shortID(ID, value string) string {
	return fmt.Sprintf("%s-%s", ID, getHash(value))
}

func getHash(text string) string {
	h := fnv.New32a()
	if _, err := h.Write([]byte(text)); err != nil {
		return text
	}
	return fmt.Sprintf("%x", h.Sum32())
}

func buildBackendIDString(protocol, serviceID, namespace, port string) string {
	return shortID(serviceID, fmt.Sprintf("%s:%s:%s:%s", protocol, serviceID, namespace, port))
}

func buildServerURLString(serviceID, namespace, port string) string {
	return fmt.Sprintf("http://%s.%s:%s", serviceID, namespace, port)
}

func buildFrontendIDString(protocol, ingressID, namespace, host, path string) string {
	return shortID(ingressID, fmt.Sprintf("%s:%s:%s:%s:%s", protocol, ingressID, namespace, host, path))
}

func buildRouteString(host, path string) string {
	return fmt.Sprintf("Host(`%s`) && Path(`%s`)", host, path)
}

func (kv *kube2vulcand) newIngress(obj interface{}) {
	if ing, ok := obj.(*kextensions.Ingress); ok {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				port := strconv.Itoa(path.Backend.ServicePort.IntVal)
				frontendID := buildFrontendIDString(frontendType, ing.Name, ing.Namespace, rule.Host, path.Path)
				backendID := buildBackendIDString(backendType, path.Backend.ServiceName, ing.Namespace, strconv.Itoa(path.Backend.ServicePort.IntVal))
				serverURL := buildServerURLString(path.Backend.ServiceName, ing.Namespace, port)
				route := buildRouteString(rule.Host, path.Path)
				if !validateRoute(route) {
					glog.Errorf("Invalid route: %v", route)
					continue
				}
				kv.mutateEtcdOrDie(func() error {
					if err := kv.addEngine(backendID, newBackend(backendID)); err != nil {
						return err
					}
					if err := kv.addEngine(backendID, newServer(serverURL)); err != nil {
						return err
					}
					return kv.addEngine(frontendID, newFrontend(frontendID, backendID, route))
				})
			}
		}
	}
}

func (kv *kube2vulcand) removeIngress(obj interface{}) {
	if ing, ok := obj.(*kextensions.Ingress); ok {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				frontendID := buildFrontendIDString(frontendType, ing.Name, ing.Namespace, rule.Host, path.Path)
				backendID := buildBackendIDString(backendType, path.Backend.ServiceName, ing.Namespace,
					strconv.Itoa(path.Backend.ServicePort.IntVal))
				kv.mutateEtcdOrDie(func() error {
					if err := kv.removeEngine(frontendID, new(frontend)); err != nil {
						return err
					}
					return kv.removeEngine(backendID, new(backend))
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

func main() {
	// TODO: replace by non k8s code / logs
	util.InitFlags()
	util.InitLogs()
	defer util.FlushLogs()

	if *argVersion {
		fmt.Println(VERSION)
		os.Exit(0)
	}
	glog.Infof("Running version %s", VERSION)

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
