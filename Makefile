# vlogs-backup 门禁与构建脚本（对应 AGENTS.md §6 验证门禁）
# 用法: make gate   依次执行全部门禁；单项可 make fmt-check / vet / lint / test / build-linux
#       make build-all   产出四平台二进制（Linux/Windows × amd64/arm64，对应 Q6 裁决）

GO ?= go
BIN_DIR := bin
BINARY_LINUX := $(BIN_DIR)/vlogs-backup-linux-amd64
TARGETS := \
	linux/amd64 \
	linux/arm64 \
	windows/amd64 \
	windows/arm64

.PHONY: gate fmt-check vet lint test build-linux build-all clean

# 全部门禁，任一失败即中止
gate: fmt-check vet lint test build-linux
	@echo "== gate passed =="

# gofmt -l . 必须无输出
fmt-check:
	@echo "== gofmt -l . =="
	@output="$$(gofmt -l .)"; \
	if [ -n "$$output" ]; then \
		echo "gofmt 未格式化文件:"; echo "$$output"; exit 1; \
	fi

vet:
	@echo "== go vet ./... =="
	$(GO) vet ./...

lint:
	@echo "== golangci-lint run =="
	golangci-lint run

test:
	@echo "== go test -race ./... =="
	$(GO) test -race ./...

build-linux:
	@echo "== CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build =="
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(BINARY_LINUX) ./cmd/vlogs-backup

# 四平台交叉编译（Q6）：Windows 产物自动带 .exe
build-all:
	@mkdir -p $(BIN_DIR)
	@for t in $(TARGETS); do \
		os=$${t%%/*}; arch=$${t##*/}; \
		name=vlogs-backup-$${os}-$${arch}; \
		if [ "$${os}" = "windows" ]; then name=$${name}.exe; fi; \
		echo "== CGO_ENABLED=0 GOOS=$${os} GOARCH=$${arch} go build =>"; \
		CGO_ENABLED=0 GOOS=$${os} GOARCH=$${arch} $(GO) build -o $(BIN_DIR)/$${name} ./cmd/vlogs-backup || exit 1; \
	done
	@echo "== build-all done =="

clean:
	rm -rf $(BIN_DIR)
