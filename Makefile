# SPDX-License-Identifier: GPL-3.0-only

APP_NAME ?= pvcguardian
IMAGE_REGISTRY ?= ghcr.io/laghoule
IMAGE_TAG ?= latest
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

GOSEC ?= gosec
GOSEC_VERSION ?= v2.30.0

.PHONY: all build test lint gosec image clean

all: test build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/pvcguardian

test:
	go test -v -race ./...

lint:
	go vet ./cmd/... ./pkg/...

gosec:
	@command -v $(GOSEC) >/dev/null 2>&1 || { \
		echo "$(GOSEC) not found. Install it with:"; \
		echo "  go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)"; \
		exit 1; \
	}
	$(GOSEC) -fmt=text ./...

image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE_REGISTRY)/$(APP_NAME):$(IMAGE_TAG) .

clean:
	rm -rf bin/
