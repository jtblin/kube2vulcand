package main

import (
	"strings"

	router "github.com/vulcand/route"
	"github.com/vulcand/vulcand/engine"
)

var validateRoute = router.NewMux().IsValid

type vulcandEngine interface {
	basePath(ID string) string
	engineType() string
	path(ID string) string
}

type backend struct {
	engine.Backend
}

func newBackend(ID string) *backend {
	return &backend{engine.Backend{Id: ID, Type: backendType}}
}

func (b *backend) engineType() string {
	return "backend"
}

func (b *backend) basePath(ID string) string {
	return vulcandPath("backends", ID)
}

func (b *backend) path(ID string) string {
	return vulcandPath("backends", ID, "backend")
}

type frontend struct {
	engine.Frontend `json:",inline"`
}

func newFrontend(ID, backendID, route string) *frontend {
	return &frontend{engine.Frontend{Id: ID, BackendId: backendID, Route: route, Type: frontendType}}
}

func (f *frontend) engineType() string {
	return "frontend"
}

func (f *frontend) basePath(ID string) string {
	return vulcandPath("frontends", ID)
}

func (f *frontend) path(ID string) string {
	return vulcandPath("frontends", ID, "frontend")
}

type server struct {
	engine.Server
}

func newServer(URL string) *server {
	return &server{engine.Server{Id: "server", URL: URL}}
}

func (s *server) engineType() string {
	return "backend server"
}

func (s *server) basePath(ID string) string {
	return vulcandPath("backends", ID)
}

func (s *server) path(ID string) string {
	return vulcandPath("backends", ID, "servers", "server")
}

func vulcandPath(keys ...string) string {
	return strings.Join(append([]string{etcdKey}, keys...), "/")
}
