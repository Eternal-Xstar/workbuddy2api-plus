# BuddyGate - 跨平台构建/运行入口（macOS / Linux / Windows 上的 make）
#
#   make build            本机构建 -> bin/buddygate
#   make cross            交叉构建 linux / darwin / windows（amd64+arm64）
#   make test             跑全部测试
#   make vet              静态检查
#   make run              前台运行（读完 config.json）
#   make clean            清理构建产物
#
# Windows 上如果没有 make，用 buddygate.ps1（功能对齐，见 README）。

BINARY  := buddygate
PKG     := ./cmd/server
VERSION := $(shell sed -n 's/^const appVersion = "\(.*\)"/\1/p' cmd/server/main.go)
LDFLAGS := -s -w
ifneq ($(VERSION),)
LDFLAGS := $(LDFLAGS) -X main.appVersion=$(VERSION)
endif
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build cross test vet run clean tidy fmt

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o bin/$(BINARY) $(PKG)
	@echo "built: bin/$(BINARY)  (version $(VERSION))"

cross:
	@mkdir -p bin
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=bin/$(BINARY)-$$os-$$arch; \
		[ "$$os" = windows ] && out=$$out.exe; \
		echo "==> $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done
	@echo "done: bin/$(BINARY)-*"

test:
	go test ./...

vet:
	go vet ./...

run: build
	./bin/$(BINARY) -config config.json

clean:
	rm -rf bin

tidy:
	go mod tidy

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './bin/*')
