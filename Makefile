GO ?= go
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
UNITDIR ?= /etc/systemd/system
CONFDIR ?= /etc/sandboxxing

.PHONY: build test vet fmt install uninstall clean

build:
	$(GO) build -trimpath -o bin/sandboxxing ./cmd/sandboxxing

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

install: build
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

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/sandboxxing
	rm -f $(DESTDIR)$(UNITDIR)/sandboxxing.service
	@echo "configuration in $(DESTDIR)$(CONFDIR) and data in /var/lib/sandboxxing were kept"

clean:
	rm -rf bin
