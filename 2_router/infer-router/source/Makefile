.PHONY: build run test test-cover proto clean lint tidy pack

BINARY=infer-router
BUILD_DIR=bin
COVER_OUT=coverage.out

# 业务代码包（排除 protobuf 生成代码和 metrics 注册器）
COVER_PKGS=./cmd/...,./internal/...,./pkg/logger/...,./pkg/jsonutil/...

# Build information
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "unknown")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
# Operator-facing build time is rendered in Beijing time (UTC+8).
BUILD_DATE ?= $(shell TZ=Asia/Shanghai date +"%Y-%m-%dT%H:%M:%S+08:00")

# ldflags for injecting build-time variables
LDFLAGS := -ldflags "-s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILD_DATE)"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/router

run: build
	$(BUILD_DIR)/$(BINARY) -mode=mixed

test:
	go test ./... -v -race

test-cover:
	@echo "==> Running tests with coverage (excluding generated code)..."
	@go test -race -count=1 -coverprofile=$(COVER_OUT) -coverpkg=$(COVER_PKGS) ./...
	@echo ""
	@echo "==> Per-package coverage:"
	@go tool cover -func=$(COVER_OUT) \
		| grep -v 'total:' \
		| awk '{ \
			n=split($$1, a, "/"); \
			file=a[n]; sub(/:.*/, "", file); \
			dir=$$1; sub(/\/[^\/]*:[0-9].*/, "", dir); \
			pct=$$NF; sub(/%/, "", pct); \
			total[dir]++; \
			pctsum[dir]+=pct; \
			if (pct+0 == 100) full[dir]++; \
		} END { \
			for (d in total) { \
				avg=pctsum[d]/total[d]; \
				printf "  %-55s %5.1f%%\n", d, avg; \
			} \
		}' | sort
	@echo ""
	@echo "==> Total:"
	@go tool cover -func=$(COVER_OUT) | tail -1

proto:
	protoc --proto_path=api/proto \
		--go_out=api/proto/routerpb --go_opt=paths=source_relative \
		--go-grpc_out=api/proto/routerpb --go-grpc_opt=paths=source_relative \
		router.proto

clean:
	rm -rf $(BUILD_DIR) $(COVER_OUT) output.tar.gz

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

pack: build
	@echo "==> Packing binary with source code..."
	@rm -rf source
	@mkdir -p source
	@rsync -av --exclude='.git' --exclude='.VSCodeCounter' --exclude='.claude' --exclude='go-perf-concurrency-workspace' --exclude='tests' --exclude='vendor' --exclude='bin' --exclude='source' --exclude='output' --exclude='cover.out' --exclude='output.tar.gz' --exclude='*.tar.gz' --exclude='infer-router' . source/
	@find source -name '*_test.go' -delete
	@tar -czf output.tar.gz bin source
	@rm -rf source
	@ls -lh output.tar.gz
