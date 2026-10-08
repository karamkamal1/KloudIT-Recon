# KloudIT Recon build
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X github.com/karamkamal1/kloudit-recon/internal/gateway.Version=$(VERSION) -X github.com/karamkamal1/kloudit-recon/internal/host.Version=$(VERSION)
GO      ?= go
DIST     = dist

.PHONY: all build gateway host windows test e2e release clean netem netem-clear netem-status helper helper-test

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
	mkdir -p $(DIST)/windows/latency-test && cp tools/latency-test/index.html $(DIST)/windows/latency-test/

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

# Helper integration tests (mock backend, the NVENC backend against its test double
# recon-fake-nvenc.dll) under Wine, against the mingw build. Wine's D3D11 needs an X
# display: run under xvfb-run (Mesa llvmpipe) to include the GPU conversion self-test,
# the synthetic-gpu pipeline test and the NVENC test; headless they skip.
# FFMPEG_DIR=<bin directory of an FFmpeg 8.x shared build with libx264> (BtbN
# ffmpeg-n8.1-latest-win64-gpl-shared-8.1) adds the libavcodec backend's stream tests.
# LANG=C.UTF-8: Wine stores non-ASCII file names (the Unicode path test) only under a
# UTF-8 Unix locale.
FFMPEG_DIR ?=
helper-test: helper
	GOOS=windows GOARCH=amd64 $(GO) test -c -o $(DIST)/obj/encoder.test.exe ./internal/host/encoder
	cd $(DIST)/obj && RECON_HELPER_EXE='Z:$(subst /,\,$(abspath $(DIST)/windows/recon-encoder.exe))' \
		RECON_FAKE_NVENC='Z:$(subst /,\,$(abspath $(HELPER_BUILD)/bin/recon-fake-nvenc.dll))' \
		$(if $(FFMPEG_DIR),RECON_FFMPEG_DIR='Z:$(subst /,\,$(abspath $(FFMPEG_DIR)))') \
		LANG=C.UTF-8 $(WINE) ./encoder.test.exe -test.v -test.count=1

# third_party/quic-go is a separate module (not in ./...): the last line runs the upstream tests
# of the packages third_party/quic-go.patch changes.
test:
	$(GO) vet ./...
	GOOS=windows $(GO) vet ./...
	$(GO) test ./...
	python3 -m unittest discover -s tools/latency-rig/test
	cd third_party/quic-go && $(GO) test ./internal/ackhandler/... ./internal/congestion/... ./congestion/...

e2e: build
	node test/e2e/browser.mjs
	node tools/latency-rig/test/flash_smoke.mjs

# Network impairment for tests (docs/NETEM.md), as root on a Linux machine in the path:
#   make netem PROFILE=lan|wifi|wan|capdrop IFACE=veth210i0 [MATCH_HOST=ip] [MATCH_PORT=8443] [MATCH_PROTO=udp]
#   make netem-clear IFACE=veth210i0        make netem-status IFACE=veth210i0
# CT=210 instead of IFACE picks the Proxmox container's veth; NETEM_ARGS passes more options.
NETEM_DEV = $(if $(IFACE),--iface $(IFACE)) $(if $(CT),--ct $(CT))
NETEM_MATCH = $(if $(MATCH_HOST),--host $(MATCH_HOST)) $(if $(MATCH_PORT),--port $(MATCH_PORT)) $(if $(MATCH_PROTO),--proto $(MATCH_PROTO))

netem:
	bash deploy/netem/netem.sh apply $(PROFILE) $(NETEM_DEV) $(NETEM_MATCH) $(NETEM_ARGS)

netem-clear:
	bash deploy/netem/netem.sh clear $(NETEM_DEV)

netem-status:
	bash deploy/netem/netem.sh status $(NETEM_DEV)

release: clean
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/gateway-linux-amd64/recon-gateway ./cmd/recon-gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/gateway-linux-arm64/recon-gateway ./cmd/recon-gateway
	for d in gateway-linux-amd64 gateway-linux-arm64; do \
		cp deploy/linux/install-gateway.sh deploy/linux/recon-gateway.service deploy/proxmox/create-lxc.sh deploy/netem/netem.sh $(DIST)/$$d/; \
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
