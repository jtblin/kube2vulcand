KUBE_CA_PATH := $(KUBE_CA_PATH)
VERSION_VAR := main.VERSION
REPO_VERSION := $(shell git describe --always --dirty --tags)
GOBUILD_VERSION_ARGS := -ldflags "-X $(VERSION_VAR)=$(REPO_VERSION)"
GIT_HASH := $(shell git rev-parse --short HEAD)
BINARY_NAME := kube2vulcand
IMAGE_NAME := jtblin/$(BINARY_NAME)

ARCH := linux darwin windows freebsd

setup:
	go get -v
	go get -v -u github.com/githubnemo/CompileDaemon
	go get -v -u github.com/alecthomas/gometalinter
	gometalinter --install --update

build: *.go fmt
	go build -o build/bin/$(BINARY_NAME) $(GOBUILD_VERSION_ARGS) github.com/jtblin/$(BINARY_NAME)

fmt:
	gofmt -w=true $(shell find . -type f -name '*.go' -not -path "./vendor/*")
	goimports -w=true -d $(shell find . -type f -name '*.go' -not -path "./vendor/*")

test:
	go test

cover:
	go test -covermode=count -coverprofile=coverage.out
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out

junit-test: build
	go get github.com/jstemmer/go-junit-report
	go test -v | go-junit-report > test-report.xml

check:
	gometalinter --deadline=10s ./...

run: build
	./build/bin/kube2vulcand --kubecfg-file=`ls ~/.kube/config` --v=1

watch:
	CompileDaemon -color=true -build "make test check"

commit-hook:
	cp dev/commit-hook.sh .git/hooks/pre-commit

cross:
	CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -X $(VERSION_VAR)=$(REPO_VERSION)" -a -installsuffix cgo -o build/bin/$(BINARY_NAME)-linux .

docker: cross
	cd build && docker build -t $(IMAGE_NAME):$(GIT_HASH) .

release: check test docker
	docker push $(IMAGE_NAME):$(GIT_HASH)
	docker tag -f $(IMAGE_NAME):$(GIT_HASH) $(IMAGE_NAME):latest
	docker push $(IMAGE_NAME):latest

run-docker: cross
	docker-compose -f build/docker-compose.yml build
	docker-compose -f build/docker-compose.yml up --force-recreate

version:
	@echo $(REPO_VERSION)

clean:
	rm -f build/bin/*
	-docker rm $(docker ps -a -f 'status=exited' -q)
	-docker rmi $(docker images -f 'dangling=true' -q)

.PHONY: build
