GO ?= go
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
UNITDIR ?= /etc/systemd/system
CONFDIR ?= /etc/sandboxxing
# CONFIG is read by "make uninstall" to learn which bridge to remove.
CONFIG ?= $(CONFDIR)/config.json
# The agent runs inside the Lambda MicroVMs, which are ARM64 only.
AGENT := deploy/microvm-image/sandboxxing-agent
CDK := cd deploy/cdk && npx cdk

.PHONY: help build agent test vet fmt install uninstall clean cdk-install cdk-synth cdk-deploy cdk-destroy

help:
	@echo "targets:"
	@echo "  build      compile bin/sandboxxing"
	@echo "  test       run the unit tests"
	@echo "  vet        run go vet"
	@echo "  fmt        format the sources"
	@echo "  install    install the pre-built binary, the unit and the config"
	@echo "  uninstall  remove the files, the bridge and the firewall rules"
	@echo "  clean      remove bin/"
	@echo "  agent      compile the MicroVM agent into deploy/microvm-image/"
	@echo "  cdk-synth  synthesize the AWS stack of the microvm backend"
	@echo "  cdk-deploy deploy it (BASE_IMAGE_VERSION=<version> is required)"
	@echo "  cdk-destroy delete it, including the home directories"

build:
	$(GO) build -trimpath -o bin/sandboxxing ./cmd/sandboxxing

agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o $(AGENT) ./cmd/sandboxxing-agent

cdk-install:
	cd deploy/cdk && npm ci

cdk-synth: agent
	$(CDK) synth -c baseImageVersion=$(or $(BASE_IMAGE_VERSION),unset)

cdk-deploy: agent
	@if [ -z "$(BASE_IMAGE_VERSION)" ]; then \
		echo "error: pass BASE_IMAGE_VERSION, see: aws lambda-microvms list-managed-microvm-image-versions --image-identifier arn:aws:lambda:<region>:aws:microvm-image:al2023-1" >&2; \
		exit 1; \
	fi
	$(CDK) deploy -c baseImageVersion=$(BASE_IMAGE_VERSION)

cdk-destroy:
	$(CDK) destroy -c baseImageVersion=$(or $(BASE_IMAGE_VERSION),unset)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# install does not build. Building under the privileges of the installer would
# run arbitrary code (go generate steps, cgo, module replace directives) as
# root, which is a supply chain risk. Build as the unprivileged user and let
# install copy the result.
install:
	@if [ ! -f bin/sandboxxing ]; then \
		echo "error: bin/sandboxxing is missing; run 'make build' as a normal user first" >&2; \
		exit 1; \
	fi
	install -Dm755 bin/sandboxxing $(DESTDIR)$(BINDIR)/sandboxxing
	install -Dm644 packaging/systemd/sandboxxing.service $(DESTDIR)$(UNITDIR)/sandboxxing.service
	install -d $(DESTDIR)$(CONFDIR)
	if [ ! -f $(DESTDIR)$(CONFDIR)/config.json ]; then \
		install -Dm644 packaging/config.example.json $(DESTDIR)$(CONFDIR)/config.json; \
	fi
	@echo "installed. next steps:"
	@echo "  sudo systemctl daemon-reload"
	@echo "  sudo sandboxxing -check"
	@echo "  sudo systemctl enable --now sandboxxing"

# uninstall stops the service, removes the host resources that the daemon
# created (the bridge and the NAT rules) and then deletes the installed files.
# The bridge name is read from the installed configuration, so a custom bridge
# is removed as well. The configuration and the container data are kept.
uninstall:
	-systemctl stop sandboxxing.service 2>/dev/null
	-systemctl disable sandboxxing.service 2>/dev/null
	@if [ -n "$(DESTDIR)" ]; then \
		echo "note: DESTDIR is set, the host resources are left untouched"; \
	elif [ -x "$(BINDIR)/sandboxxing" ]; then \
		"$(BINDIR)/sandboxxing" -config "$(CONFIG)" -cleanup || \
			echo "warning: could not remove every host resource; check 'ip link show' and 'nft list tables'" >&2; \
	else \
		echo "note: $(BINDIR)/sandboxxing is already gone; falling back to ip/nft"; \
		nft delete table ip sandboxxing 2>/dev/null || true; \
		ip link delete sbx0 2>/dev/null || true; \
	fi
	-rm -f "$(DESTDIR)$(BINDIR)/sandboxxing"
	-rm -f "$(DESTDIR)$(UNITDIR)/sandboxxing.service"
	-systemctl daemon-reload 2>/dev/null
	@echo "configuration in $(DESTDIR)$(CONFDIR) and data in /var/lib/sandboxxing were kept"

clean:
	rm -rf bin $(AGENT)
