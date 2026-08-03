VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST    := dist

.PHONY: all build test check server sgctl dist clean

all: check build

build: server sgctl

server:
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o bin/shiguang ./cmd/server

sgctl:
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o bin/sgctl ./cmd/sgctl

test:
	go test ./...

check:
	go build ./... && go vet ./... && go test ./...

# 交叉编译 sgctl：批量导入工具要放到有照片的那台机器上跑，
# 所以各平台都出一份。纯 Go、无 CGO，交叉编译开箱即用。
dist:
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@set -e; for target in \
		darwin/amd64 darwin/arm64 \
		linux/amd64 linux/arm64 \
		windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		out=$(DIST)/sgctl-$$os-$$arch; \
		[ "$$os" = "windows" ] && out=$$out.exe; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -trimpath -ldflags='$(LDFLAGS)' -o $$out ./cmd/sgctl; \
	done
	@echo; ls -lh $(DIST)

clean:
	rm -rf bin $(DIST)
