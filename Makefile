# KloudIT Recon build
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X github.com/karamkamal1/kloudit-recon/internal/gateway.Version=$(VERSION) -X github.com/karamkamal1/kloudit-recon/internal/host.Version=$(VERSION)
GO      ?= go
DIST     = dist

.PHONY: all build gateway host windows helper helper-test test e2e release clean

all: build

build: gateway host

gateway:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/recon-gateway ./cmd/recon-gateway

host:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/recon-host ./cmd/recon-host

# Windows host agent: console build (pair/probe) + GUI-subsystem build (background task, no window).
windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/windows/recon-host.exe ./cmd/recon-host
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o $(DIST)/windows/recon-hostw.exe ./cmd/recon-host
	cp deploy/windows/*.ps1 $(DIST)/windows/

# Native capture/encode helper recon-encoder.exe (native/recon-encoder), cross-compiled
# with mingw-w64 (apt-get install mingw-w64 cmake). Skipped when the toolchain is missing,
# unless HELPER_REQUIRED=1. The mingw build has no Windows.Graphics.Capture (no C++/WinRT):
# releases ship the MSVC build from CI instead (make release HELPER_EXE=path/to/recon-encoder.exe).
MINGW_CXX       ?= x86_64-w64-mingw32-g++
HELPER_BUILD     = $(DIST)/obj/recon-encoder
HELPER_REQUIRED ?= 0
HELPER_EXE      ?=
WINE            ?= wine

helper:
	@if command -v $(MINGW_CXX) >/dev/null 2>&1 && command -v cmake >/dev/null 2>&1; then \
		cmake -S native/recon-encoder -B $(HELPER_BUILD) -DCMAKE_BUILD_TYPE=Release \
			-DCMAKE_TOOLCHAIN_FILE=$(CURDIR)/native/recon-encoder/cmake/mingw-w64-x86_64.cmake >/dev/null && \
		cmake --build $(HELPER_BUILD) --parallel && \
		mkdir -p $(DIST)/windows && cp $(HELPER_BUILD)/bin/recon-encoder.exe $(DIST)/windows/; \
	elif [ "$(HELPER_REQUIRED)" = 1 ]; then \
		echo "helper: mingw-w64 ($(MINGW_CXX)) and cmake are required" >&2; exit 1; \
	else \
		echo "helper: mingw-w64 ($(MINGW_CXX)) or cmake not found, skipping recon-encoder.exe"; \
	fi

# Helper integration tests (mock backend) under Wine, against the mingw build. Wine's
# D3D11 needs an X display: run under xvfb-run (Mesa llvmpipe) to include the GPU
# conversion self-test and the synthetic-gpu pipeline test; headless they skip.
helper-test: helper
	GOOS=windows GOARCH=amd64 $(GO) test -c -o $(DIST)/obj/encoder.test.exe ./internal/host/encoder
	cd $(DIST)/obj && RECON_HELPER_EXE='Z:$(subst /,\,$(abspath $(DIST)/windows/recon-encoder.exe))' \
		$(WINE) ./encoder.test.exe -test.v -test.count=1

test:
	$(GO) vet ./...
	GOOS=windows $(GO) vet ./...
	$(GO) test ./...

e2e: build
	node test/e2e/browser.mjs

release: clean
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/gateway-linux-amd64/recon-gateway ./cmd/recon-gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/gateway-linux-arm64/recon-gateway ./cmd/recon-gateway
	for d in gateway-linux-amd64 gateway-linux-arm64; do \
		cp deploy/linux/install-gateway.sh deploy/linux/recon-gateway.service deploy/proxmox/create-lxc.sh $(DIST)/$$d/; \
		tar -C $(DIST) -czf $(DIST)/kloudit-recon-$(VERSION)-$$d.tar.gz $$d; \
	done
	$(MAKE) windows
	@if [ -n "$(HELPER_EXE)" ]; then \
		echo "helper: using $(HELPER_EXE)"; cp "$(HELPER_EXE)" $(DIST)/windows/recon-encoder.exe; \
	else \
		$(MAKE) helper; \
	fi
	cd $(DIST) && mv windows host-windows-amd64 && zip -qr kloudit-recon-$(VERSION)-host-windows-amd64.zip host-windows-amd64
	cd $(DIST) && sha256sum *.tar.gz *.zip > SHA256SUMS

clean:
	rm -rf $(DIST)
