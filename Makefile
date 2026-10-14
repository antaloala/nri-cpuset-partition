BINARY  ?= nri-cpuset-partition
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
IMAGE   ?= ghcr.io/example/$(BINARY)
TAG     ?= $(VERSION)
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w -X main.version=$(VERSION)

.PHONY: all
all: check build

.PHONY: build
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

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

.PHONY: image
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

.PHONY: deploy
deploy:
	kubectl apply -f deploy/daemonset.yaml

.PHONY: logs
logs:
	kubectl -n nri-cpuset-partition logs -l app.kubernetes.io/name=$(BINARY) -f

.PHONY: clean
clean:
	rm -rf bin
