# KloudIT Recon build
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X github.com/karamkamal1/kloudit-recon/internal/gateway.Version=$(VERSION) -X github.com/karamkamal1/kloudit-recon/internal/host.Version=$(VERSION)
GO      ?= go
DIST     = dist

.PHONY: all build gateway host windows test e2e release clean

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
	cd $(DIST) && mv windows host-windows-amd64 && zip -qr kloudit-recon-$(VERSION)-host-windows-amd64.zip host-windows-amd64
	cd $(DIST) && sha256sum *.tar.gz *.zip > SHA256SUMS

clean:
	rm -rf $(DIST)
