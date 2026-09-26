# Code is edited on the Mac and cross-compiled for the Linux VM, where it
# runs as root. VM names the Lima instance used for tests.
VM      ?= crt1
GOARCH  ?= arm64
GO      := GOTOOLCHAIN=local GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go
BINS    := mycrt myagent mycontrol myctl
# Packages whose tests need Linux and root; built as test binaries.
LINUX_TESTS := image

# Set MYCRT_REGISTRY_MIRROR=mirror.gcr.io to spare Docker Hub's rate limit.
MYCRT_REGISTRY_MIRROR ?=

.PHONY: build vet unit install clean test-%
build:
	@mkdir -p bin
	@for b in $(BINS); do $(GO) build -trimpath -o bin/$$b ./cmd/$$b || exit 1; done
	@for p in $(LINUX_TESTS); do $(GO) test -c -o bin/$$p.test ./internal/$$p || exit 1; done

vet:
	$(GO) vet ./...

# Portable unit tests, run on the Mac.
unit:
	GOTOOLCHAIN=local go test -race ./internal/units ./internal/image ./internal/registry ./internal/network ./internal/orch/...

# Copy the binaries onto the VM's own disk. Running them from the shared
# folder works but is slower, and mycrt re-executes itself per container.
install: build
	limactl shell $(VM) sudo install -m 0755 $(addprefix bin/,$(BINS)) /usr/local/bin/

# make test-stage1 builds, installs, then runs test/stage1.sh as root in the VM.
test-%: install
	limactl shell $(VM) sudo env MYCRT_REGISTRY_MIRROR=$(MYCRT_REGISTRY_MIRROR) bash test/$*.sh

clean:
	rm -rf bin
