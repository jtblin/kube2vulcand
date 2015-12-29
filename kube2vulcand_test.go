/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"testing"
	"time"

	"strconv"

	"github.com/coreos/go-etcd/etcd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vulcand/vulcand/engine"
	kapi "k8s.io/kubernetes/pkg/api"
	kextensions "k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/util"
)

const basePath = "/vulcand"

type fakeEtcdClient struct {
	writes map[string]string
}

func (ec *fakeEtcdClient) Set(key, value string, ttl uint64) (*etcd.Response, error) {
	ec.writes[key] = value
	return nil, nil
}

func (ec *fakeEtcdClient) Delete(key string, recursive bool) (*etcd.Response, error) {
	for p := range ec.writes {
		if (recursive && strings.HasPrefix(p, key)) || (!recursive && p == key) {
			delete(ec.writes, p)
		}
	}
	return nil, nil
}

func (ec *fakeEtcdClient) RawGet(key string, sort, recursive bool) (*etcd.RawResponse, error) {
	values := ec.Get(key)
	if len(values) == 0 {
		return &etcd.RawResponse{StatusCode: http.StatusNotFound}, nil
	}
	return &etcd.RawResponse{StatusCode: http.StatusOK}, nil
}

func (ec *fakeEtcdClient) Get(key string) []string {
	values := make([]string, 0, 10)
	minSeparatorCount := 0
	key = strings.ToLower(key)
	for path := range ec.writes {
		if strings.HasPrefix(path, key) {
			separatorCount := strings.Count(path, "/")
			if minSeparatorCount == 0 || separatorCount < minSeparatorCount {
				minSeparatorCount = separatorCount
				values = values[:0]
				values = append(values, ec.writes[path])
			} else if separatorCount == minSeparatorCount {
				values = append(values, ec.writes[path])
			}
		}
	}
	return values
}

func newKube2Vulcand(ec etcdClient) *kube2vulcand {
	return &kube2vulcand{
		etcdClient:          ec,
		etcdMutationTimeout: time.Second,
		ingressesStore:      cache.NewStore(cache.MetaNamespaceKeyFunc),
	}
}

func getEtcdPathForBackend(ID string) string {
	return path.Join(basePath, "backends", ID, "backend")
}

func getEtcdPathForServer(ID string) string {
	return path.Join(basePath, "backends", ID, "servers", "server")
}

func getEtcdPathForFrontend(ID string) string {
	return path.Join(basePath, "frontends", ID, "frontend")
}

func getBackendFromString(data string) (*engine.Backend, error) {
	var res engine.Backend
	err := json.Unmarshal([]byte(data), &res)
	return &res, err
}

func getServerFromString(data string) (*engine.Server, error) {
	var res engine.Server
	err := json.Unmarshal([]byte(data), &res)
	return &res, err
}

func getFrontendFromString(data string) (*engine.Frontend, error) {
	var res engine.Frontend
	err := json.Unmarshal([]byte(data), &res)
	return &res, err
}

func assertBackendEntryInEtcd(t *testing.T, ec *fakeEtcdClient, backendID, expectedType string) {
	key := getEtcdPathForBackend(backendID)
	values := ec.Get(key)
	require.True(t, len(values) > 0, "entry not found.")
	actualBackend, err := getBackendFromString(values[0])
	require.NoError(t, err)
	assert.Equal(t, expectedType, actualBackend.Type)
}

func assertServerEntryInEtcd(t *testing.T, ec *fakeEtcdClient, backendID, expectedServerURL string) {
	srvKey := getEtcdPathForServer(backendID)
	values := ec.Get(srvKey)
	assert.Equal(t, 1, len(values))
	actualServer, err := getServerFromString(values[0])
	require.NoError(t, err)
	assert.Equal(t, expectedServerURL, actualServer.URL)
}

func assertFrontendEntryInEtcd(t *testing.T, ec *fakeEtcdClient, frontendID, expectedBackendID, expectedType, expectedRoute string) {
	key := getEtcdPathForFrontend(frontendID)
	values := ec.Get(key)
	require.True(t, len(values) > 0, "entry not found.")
	actualFrontend, err := getFrontendFromString(values[0])
	require.NoError(t, err)
	assert.Equal(t, frontendID, actualFrontend.Id)
	assert.Equal(t, expectedBackendID, actualFrontend.BackendId)
	assert.Equal(t, expectedType, actualFrontend.Type)
	assert.Equal(t, expectedRoute, actualFrontend.Route)
}

func newIngress(namespace, ingressName, host, serviceName, path string, servicePort int) kextensions.Ingress {
	return kextensions.Ingress{
		ObjectMeta: kapi.ObjectMeta{
			Name:      ingressName,
			Namespace: namespace,
		},
		Spec: kextensions.IngressSpec{
			Rules: []kextensions.IngressRule{{
				Host: host,
				IngressRuleValue: kextensions.IngressRuleValue{HTTP: &kextensions.HTTPIngressRuleValue{
					Paths: []kextensions.HTTPIngressPath{{Path: path, Backend: kextensions.IngressBackend{
						ServiceName: serviceName,
						ServicePort: util.IntOrString{IntVal: servicePort},
					}}},
				}},
			}},
		},
	}
}

func TestIngress(t *testing.T) {
	const (
		testIngress   = "testingress"
		testService   = "http1"
		testNamespace = "default"
		testHost      = "foo.example.com"
		testPath      = "/"
		testPort      = "80"
	)
	ec := &fakeEtcdClient{make(map[string]string)}
	k2v := newKube2Vulcand(ec)

	// create ingress
	intPort, _ := strconv.Atoi(testPort)
	ingress := newIngress(testNamespace, testIngress, testHost, testService, testPath, intPort)
	expectedBackendID := buildBackendIDString(backendType, testService, testNamespace, testPort)
	expectedServerURL := buildServerURLString(testService, testNamespace, testPort)
	expectedFrontendID := buildFrontendIDString(frontendType, testIngress, testNamespace, testHost, testPath)
	expectedRoute := buildRouteString(testHost, testPath)

	k2v.newIngress(&ingress)
	assertBackendEntryInEtcd(t, ec, expectedBackendID, backendType)
	assertServerEntryInEtcd(t, ec, expectedBackendID, expectedServerURL)
	assertFrontendEntryInEtcd(t, ec, expectedFrontendID, expectedBackendID, frontendType, expectedRoute)

	// Doesn't work since glide or 1.5.2
	//	assert.Len(t, ec.writes, 3)
	assert.Equal(t, 3, len(ec.writes))

	// update ingress
	newIngress := newIngress(testNamespace, testIngress, "bar.example.com", "http2", testPath, intPort)
	expectedBackendID = buildBackendIDString(backendType, "http2", testNamespace, testPort)
	expectedServerURL = buildServerURLString("http2", testNamespace, testPort)
	expectedFrontendID = buildFrontendIDString(frontendType, testIngress, testNamespace, "bar.example.com", testPath)
	expectedRoute = buildRouteString("bar.example.com", testPath)

	k2v.updateIngress(&ingress, &newIngress)
	assertBackendEntryInEtcd(t, ec, expectedBackendID, backendType)
	assertServerEntryInEtcd(t, ec, expectedBackendID, expectedServerURL)
	assertFrontendEntryInEtcd(t, ec, expectedFrontendID, expectedBackendID, frontendType, expectedRoute)

	// Doesn't work since glide or 1.5.2
	//	assert.Len(t, ec.writes, 3)
	assert.Equal(t, 3, len(ec.writes))

	// Delete the ingress
	k2v.removeIngress(&newIngress)
	assert.Empty(t, ec.writes)
}

func TestBuildFrontendID(t *testing.T) {
	expectedFrontendID := "name-e77ae913"
	assert.Equal(t, expectedFrontendID, buildFrontendIDString("http", "name", "default", "foo.example.com", "/"))
}

func TestBuildBackendID(t *testing.T) {
	expectedBackendID := "name-92b94b41"
	assert.Equal(t, expectedBackendID, buildBackendIDString("http", "name", "default", "80"))
}

func TestBuildServerURL(t *testing.T) {
	expectedServerURL := "http://name.default:80"
	assert.Equal(t, expectedServerURL, buildServerURLString("name", "default", "80"))
}

func TestBuildRoute(t *testing.T) {
	expectedRoute := "Host(`name`) && PathRegexp(`/.*`)"
	assert.Equal(t, expectedRoute, buildRouteString("name", "/"))
}
