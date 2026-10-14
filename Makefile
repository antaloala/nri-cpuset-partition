BINARY  ?= nri-cpuset-partition
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
IMAGE   ?= ghcr.io/example/$(BINARY)
TAG     ?= $(VERSION)
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w -X main.version=$(VERSION)
# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= podman

.PHONY: all
all: check build

.PHONY: build
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/

.PHONY: check
check: fmt vet test

.PHONY: fmt
fmt:
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt needed:"; gofmt -l cmd internal; exit 1; }

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test -race -cover ./...


.PHONY: image-build
image-build:
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) --platform linux/amd64 .

.PHONY: image-push
image-push: ## Push container image.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: deploy
deploy:
	kubectl apply -f deploy/daemonset.yaml

.PHONY: logs
logs:
	kubectl -n nri-cpuset-partition logs -l app.kubernetes.io/name=$(BINARY) -f

.PHONY: clean
clean:
	rm -rf bin
