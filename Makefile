# KloudIT Recon build
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X github.com/karamkamal1/kloudit-recon/internal/gateway.Version=$(VERSION) -X github.com/karamkamal1/kloudit-recon/internal/host.Version=$(VERSION)
GO      ?= go
DIST     = dist

.PHONY: all build gateway host windows test e2e release clean netem netem-clear netem-status

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

test:
	$(GO) vet ./...
	GOOS=windows $(GO) vet ./...
	$(GO) test ./...
	python3 -m unittest discover -s tools/latency-rig/test

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
	cd $(DIST) && mv windows host-windows-amd64 && zip -qr kloudit-recon-$(VERSION)-host-windows-amd64.zip host-windows-amd64
	cd $(DIST) && sha256sum *.tar.gz *.zip > SHA256SUMS

clean:
	rm -rf $(DIST)
