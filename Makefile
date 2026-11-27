# keenetic-smb-ui — build helpers
#
# Cross-compiles a static Go binary for every Entware architecture Keenetic
# routers use, then packs .ipk packages for opkg.

APP      := keenetic-smb-ui
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOOS     ?= linux
GO       ?= go
ARCH     ?=
OUT      := dist

TARGETS := \
	linux/arm64 \
	linux/arm \
	linux/mipsle \
	linux/mips \
	linux/mips64le \
	linux/386 \
	linux/amd64

ifneq ($(ARCH),)
TARGETS := $(GOOS)/$(ARCH)
endif

.PHONY: all build tidy test fmt vet clean ipk

all: build

tidy:
	go mod tidy

fmt:
	gofmt -l -w cmd internal

vet:
	go vet ./...

test:
	$(GO) test -timeout 20m ./...

build:
	mkdir -p $(OUT)
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			$(GO) build -trimpath -ldflags "-s -w -X github.com/tommilevs/keenetic-smb-ui/internal/web.Version=$(VERSION)" \
			-o $(OUT)/$(APP)_$${os}_$${arch} ./cmd/keenetic-smb-ui || exit 1; \
	done
	@ls -lh $(OUT)

ipk: build
	sh scripts/build-ipk.sh "$(VERSION)" "$(ARCH)"

clean:
	rm -rf $(OUT) build
