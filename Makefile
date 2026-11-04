GO ?= go
BIN := priyomnik
PREFIX ?= /usr/local
DESTDIR ?=
BINDIR ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share
APPLICATIONSDIR ?= $(DATADIR)/applications
ICONSDIR ?= $(DATADIR)/icons/hicolor

.PHONY: all build install uninstall test race vet fmt clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath -ldflags="-s -w" -o $(BIN) .

install: build
	install -Dm755 $(BIN) "$(DESTDIR)$(BINDIR)/$(BIN)"
	install -Dm644 assets/priyomnik.desktop "$(DESTDIR)$(APPLICATIONSDIR)/priyomnik.desktop"
	install -Dm644 assets/icons/priyomnik.png "$(DESTDIR)$(ICONSDIR)/256x256/apps/priyomnik.png"

uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/$(BIN)"
	rm -f "$(DESTDIR)$(APPLICATIONSDIR)/priyomnik.desktop"
	rm -f "$(DESTDIR)$(ICONSDIR)/256x256/apps/priyomnik.png"

test:
	$(GO) test -mod=readonly ./...

race:
	$(GO) test -mod=readonly -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BIN)
