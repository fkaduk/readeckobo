GNUARCH ?= $(shell arch)
BINARY  ?= build/kobodeck.$(GNUARCH)
TARBALL_BINARY ?= build/kobodeck.arm
TARBALL ?= build/KoboRoot.tgz
BUILD_FLAGS ?=
COVERAGE_ENABLED ?=
SOURCES  = $(wildcard *.go) go.mod go.sum kobodeck.toml Makefile
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct)
GO_CACHE_DIR ?= /tmp/kobodeck-go-build
GO_MOD_CACHE_DIR ?= /tmp/kobodeck-go-mod
GOENV ?= /tmp/kobodeck-go-env
GOLANGCI_LINT_CACHE ?= /tmp/kobodeck-golangci-lint
export GOENV GOLANGCI_LINT_CACHE

GFLAGS += -trimpath -ldflags="-s -w -X main.buildVersion=$(shell git describe --always --dirty --tags)" $(BUILD_FLAGS)
CROSS_COMPILE_FLAGS = GOARCH=arm GOOS=linux CGO_ENABLED=0

.PHONY: all agent-init tarball build tag clean check ci coverage lint fmt fmt-check mod-check test test-e2e vulncheck
.NOTPARALLEL: ci

all: tarball

agent-init:
	go env -w GOCACHE=$(GO_CACHE_DIR) GOMODCACHE=$(GO_MOD_CACHE_DIR)
	@echo Go build cache: $(GO_CACHE_DIR)
	@echo Go module cache: $(GO_MOD_CACHE_DIR)

tarball:
	@echo building Kobo tarball
	$(MAKE) -B build BINARY=$(TARBALL_BINARY) BUILD_FLAGS="$(BUILD_FLAGS)" $(CROSS_COMPILE_FLAGS)
	mkdir -p $(dir $(TARBALL)) root/usr/local/bin
	cp $(TARBALL_BINARY) root/usr/local/bin/kobodeck
	tar --sort=name --mtime='@$(SOURCE_DATE_EPOCH)' \
		--owner=0 --group=0 --numeric-owner --mode='u+rwX,go+rX,go-w' \
		-C root/ -c -f $(TARBALL:.tgz=.tar) etc usr
	gzip -n -f $(TARBALL:.tgz=.tar)
	mv $(TARBALL:.tgz=.tar).gz $(TARBALL)
	rm root/usr/local/bin/kobodeck

build: $(BINARY)

$(BINARY): $(SOURCES)
	mkdir -p $$(dirname $(BINARY))
	CGO_ENABLED=0 go build $(GFLAGS) -o $@

tag:
	@test -z "$$(git status --porcelain)" || (echo "error: working tree is dirty"; exit 1)
	@read -p "Version (e.g. v2.0.0): " v && \
	  echo "Tagging $$v at $$(git rev-parse --short HEAD)" && \
	  read -p "Push to origin? [y/N] " confirm && [ "$$confirm" = "y" ] && \
	  git tag $$v && git push origin $$v

clean:
	rm -rf ./build/e2e-coverdata ./build/merged-coverdata ./build/unit-coverdata
	rm -f ./build/*

check: lint fmt-check mod-check test vulncheck

ci: check test-e2e coverage

coverage: test
	$(MAKE) test-e2e \
		TARBALL_BINARY=build/kobodeck.cover.arm \
		TARBALL=build/KoboRoot.cover.tgz \
		BUILD_FLAGS="-cover -covermode=atomic" \
		COVERAGE_ENABLED=1
	@for input in build/unit-coverdata build/e2e-coverdata; do \
		[ -d "$$input" ] || { echo "coverage: missing input directory $$input" >&2; exit 1; }; \
		find "$$input" -maxdepth 1 -type f -name 'covmeta.*' -print -quit | grep -q . || \
			{ echo "coverage: $$input has no coverage metadata" >&2; exit 1; }; \
		find "$$input" -maxdepth 1 -type f -name 'covcounters.*' -print -quit | grep -q . || \
			{ echo "coverage: $$input has no coverage counters" >&2; exit 1; }; \
	done
	rm -rf build/merged-coverdata
	mkdir -p build/merged-coverdata
	go tool covdata merge -pcombine \
		-i=build/unit-coverdata,build/e2e-coverdata \
		-o=build/merged-coverdata
	go tool covdata textfmt -i=build/merged-coverdata -o=build/coverage.out
	@test -s build/coverage.out || { echo "coverage: merged profile is empty" >&2; exit 1; }
	go tool cover -func=build/coverage.out

lint:
	mkdir -p $(GOLANGCI_LINT_CACHE)
	go tool -modfile=actionlint.mod actionlint
	go tool -modfile=tools.mod golangci-lint run ./...
	docker run --rm \
		--env NPM_CONFIG_UPDATE_NOTIFIER=false \
		--volume "$(CURDIR):/workspace:ro" \
		--workdir /workspace \
		node:22-alpine \
		npx --yes markdownlint-cli@0.49.1 README.md
	docker run --rm \
		--volume "$(CURDIR):/workspace:ro" \
		--workdir /workspace \
		koalaman/shellcheck-alpine:v0.11.0 \
		shellcheck vm_test.sh vm_test_guest.sh

fmt:
	gofmt -s -w .
	docker run --rm \
		--user "$$(id -u):$$(id -g)" \
		--volume "$(CURDIR):/workspace" \
		--workdir /workspace \
		mvdan/shfmt:v3.13.1 \
		-w vm_test.sh vm_test_guest.sh

fmt-check:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt: these files need formatting:"; echo "$$out"; exit 1; fi
	docker run --rm \
		--volume "$(CURDIR):/workspace:ro" \
		--workdir /workspace \
		mvdan/shfmt:v3.13.1 \
		-d vm_test.sh vm_test_guest.sh

mod-check:
	go mod tidy -diff

test:
	@packages=$$(go list ./...) || exit; \
		count=$$(printf '%s\n' "$$packages" | wc -l); \
		[ "$$count" -eq 1 ] || { \
			echo "test: raw coverage requires exactly one Go package; found $$count" >&2; \
			exit 1; \
		}
	rm -rf build/unit-coverdata
	mkdir -p build/unit-coverdata
	CGO_ENABLED=1 go test -race -cover -covermode=atomic . \
		-args -test.gocoverdir=$(CURDIR)/build/unit-coverdata
	go tool covdata textfmt -i=build/unit-coverdata -o=build/unit-coverage.out
	@test -s build/unit-coverage.out || { echo "test: unit coverage profile is empty" >&2; exit 1; }

vulncheck:
	go tool -modfile=tools.mod govulncheck ./...

test-e2e: tarball
	KOBODECK_COVERAGE=$(COVERAGE_ENABLED) \
		./vm_test.sh
