# SPDX-License-Identifier: GPL-3.0-only

APP_NAME ?= pvcguardian
IMAGE_REGISTRY ?= ghcr.io/laghoule
IMAGE_TAG ?= latest
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build test lint image clean

all: test build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/pvcguardian

test:
	go test -v -race ./...

lint:
	go vet ./cmd/... ./pkg/...

image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE_REGISTRY)/$(APP_NAME):$(IMAGE_TAG) .

clean:
	rm -rf bin/
