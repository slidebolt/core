# Core Service Makefile

IMAGE_NAME ?= slidebolt-core
TAG ?= latest

.PHONY: build test stage docker-build-local docker-build-prod clean

# Local build using go.work
build:
	@echo "Building Core binary..."
	@mkdir -p bin
	go build -o bin/core ./cmd/main.go

test:
	@echo "Running tests..."
	go test ./...

# Pre-GitHub Bridge: Stage siblings so Docker can see them
stage:
	@echo "Staging sibling modules for Docker build..."
	@mkdir -p .stage
	@for mod in plugin-sdk plugin-framework plugin-loader; do \
		echo "Staging $$mod..."; \
		rm -rf .stage/$$mod; \
		cp -r ../$$mod .stage/$$mod; \
		rm -rf .stage/$$mod/.git; \
	done

# Build for local development (uses staged siblings)
docker-build-local: stage
	@echo "Building LOCAL Docker image $(IMAGE_NAME):$(TAG)..."
	docker build --build-arg BUILD_MODE=local -t $(IMAGE_NAME):$(TAG) .
	@echo "Cleaning up stage..."
	@rm -rf .stage

# Build for production (uses remote GitHub modules)
docker-build-prod:
	@echo "Building PROD Docker image $(IMAGE_NAME):$(TAG)..."
	docker build --build-arg BUILD_MODE=prod -t $(IMAGE_NAME):$(TAG) .

clean:
	@rm -rf bin/ .stage/ core
