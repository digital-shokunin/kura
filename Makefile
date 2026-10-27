PREFIX   ?= $(HOME)/.local
BINDIR   := $(PREFIX)/bin
IDENTITY ?= kura-codesign
SIGN_ID  ?= kura
VERSION  ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: build test install codesign-setup linux clean

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/kura ./cmd/kura
ifeq ($(shell uname),Darwin)
	codesign --force --sign "$(IDENTITY)" --identifier $(SIGN_ID) bin/kura
endif
	ln -sf kura bin/git-remote-kura

test:
	go vet ./...
	go test -race ./...

install: build
	install -d $(BINDIR)
	install -m 0755 bin/kura $(BINDIR)/kura
	ln -sf kura $(BINDIR)/git-remote-kura

codesign-setup:
	sh scripts/codesign-setup.sh $(IDENTITY)

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/kura-linux-amd64 ./cmd/kura

clean:
	rm -rf bin
