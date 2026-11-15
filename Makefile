.PHONY: test test-race cover fuzz image image-server
ifeq ($(shell go env GOOS),windows)
EXE=.exe
else
EXE=
endif

DIST=dist
BINDIR=.

BASENAME=$(notdir $(shell pwd))
PROGRAM=$(BINDIR)/$(BASENAME)$(EXE)
LAST_RELEASE=

REPO=$(shell go list | head -n 1)
IMAGE=$(BASENAME)
VERSION ?= $(shell ./utils/version.sh)
GO_VERSION ?= $(shell grep '^go ' go.mod | awk '{print $$2}')
DOCKER=docker
PACKAGE=$(DIST)/$(basename $(notdir $(PROGRAM)))-$(shell go env GOOS)-$(shell go env GOARCH).zip


.PHONY: $(PROGRAM)

all: $(PROGRAM)

compile: $(PROGRAM)

$(PROGRAM): $(BINDIR)
	mkdir -p $(dir $@)
	go build -ldflags="-X '$(REPO)/program.Version=${VERSION}'" -o $(PROGRAM)

package: $(PACKAGE)

$(PACKAGE): $(PROGRAM)

# These next 2 recipes know how to make .zip and .tar files, which are used implicitly in making the package
%.zip:
	mkdir $(dir $@)
	zip -j $@ $?

%.tar.gz %.tgz:
	mkdir $(dir $@)
	tar -czf $@ -C $(dir $<) $(notdir $<)



install:
	go install -ldflags="-X '$(REPO)/program.Version=${VERSION}'"


# `image` builds the full worker image (it can also run `server`).
# `image-server` builds the minimal distroless server-only image.
image:
	$(DOCKER) build --target worker --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION) -t $(IMAGE) .

image-server:
	$(DOCKER) build --target server --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION) -t $(IMAGE)-server .

test:
	go test ./...

test-race:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# Short fuzz pass over the hostile-input parsers.
fuzz:
	go test ./internal/media -run=^$$ -fuzz=FuzzWAV -fuzztime=30s
	go test ./internal/media -run=^$$ -fuzz=FuzzFLAC -fuzztime=30s
	go test ./internal/auth -run=^$$ -fuzz=FuzzParseToken -fuzztime=15s

vet:
	go vet ./...

changelog: CHANGELOG.md
CHANGELOG.md: .chglog/config.yml
	git chglog $(LAST_RELEASE) >$@

.chglog/config.yml: go.mod
	sed -i.bak -e "s|repository_url:.*|repository_url: https://$(REPO)|" $@

hooks: .git/hooks/pre-commit

.git/hooks/pre-commit: .pre-commit-config.yaml
	pre-commit install
	pre-commit install --hook-type commit-msg


info::
	@echo BASENAME=$(BASENAME)
	@echo PROGRAM=$(PROGRAM)
	@echo IMAGE=$(IMAGE)


update-repo:
	./utils/update-repo.sh $(NEWREPO)

tools:
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install github.com/go-critic/go-critic/cmd/gocritic@latest
	go install github.com/securego/gosec/v2/cmd/gosec@latest

# Real-engine integration test (internal/e2e/integration_test.go): server and
# worker containers from INTEGRATION_IMAGE running this tree's binary, real
# whisper-cli and ffmpeg, tiny.en and jfk.wav from .cache/models/. Builds the
# image first if it is missing.
INTEGRATION_IMAGE ?= whisper-network:dev
.PHONY: integration
integration:
	$(DOCKER) image inspect $(INTEGRATION_IMAGE) >/dev/null 2>&1 || $(MAKE) image IMAGE=$(INTEGRATION_IMAGE)
	WN_IT_IMAGE=$(INTEGRATION_IMAGE) go test -tags integration -run TestIntegration -count=1 -v -timeout 15m ./internal/e2e/
