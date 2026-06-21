# ============================================================
#  grpc-gateway-demo Makefile
#  同进程内同时暴露 gRPC(:5568) + HTTP REST(:8080)
# ============================================================

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOCLEAN := $(GOCMD) clean
GOTEST := $(GOCMD) test
GOMOD := $(GOCMD) mod
GOFUMPT := gofumpt
GOLINT := golangci-lint

# Project parameters
BIN_DIR := bin
BINARY_NAME := grpc-svr
MAIN_PKG := ./cmd/server
GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*' -not -path './gen/*')
PKG_LIST := $(shell $(GOCMD) list ./... | grep -v /vendor/)

# 运行端口（仅用于 help 展示，真实值在 cmd/server/main.go 中定义）
GRPC_ADDR := :5568
HTTP_ADDR := :8080

# Build flags
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d %H:%M:%S')

# Debug configuration
DEBUG ?= false
ifeq ($(DEBUG),true)
    LDFLAGS :=
    GCFLAGS := -gcflags="all=-N -l"
else
    LDFLAGS := -w -s
    GCFLAGS :=
endif

# 纯 Go 项目，关闭 CGO 以获得静态二进制、便于交叉编译
CGO_FLAGS := CGO_ENABLED=0

# Colors for pretty printing
GREEN := \033[0;32m
BLUE := \033[0;34m
YELLOW := \033[0;33m
NC := \033[0m # No Color

# Targets
.PHONY: all build compile debug proto install-tools run test fumpt lint tidy clean help

# Default target
all: build

# Full build pipeline
build: clean tidy fumpt lint $(BIN_DIR)/$(BINARY_NAME)
	@printf "$(GREEN)✓ Build completed successfully!$(NC)\n"

# Quick build without linting
compile: tidy $(BIN_DIR)/$(BINARY_NAME)
	@printf "$(GREEN)✓ Compile completed!$(NC)\n"

# Debug build target
debug:
	@printf "$(YELLOW)Building in DEBUG mode...$(NC)\n"
	@$(MAKE) DEBUG=true $(BIN_DIR)/$(BINARY_NAME)

# 生成 protobuf / gRPC / gateway 代码（pb.go / grpc.pb.go / gw.pb.go）
proto:
	@printf "$(BLUE)Generating protobuf code via generate.sh ...$(NC)\n"
	@./scripts/generate.sh

# 安装 proto 代码生成所需的 protoc 插件
install-tools:
	@printf "$(BLUE)Installing protoc plugins ...$(NC)\n"
	@$(GOCMD) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@$(GOCMD) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@$(GOCMD) install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
	@printf "$(GREEN)✓ Tools installed. 确保 \$$(go env GOPATH)/bin 已加入 PATH$(NC)\n"

# Clean build artifacts
clean:
	@printf "$(YELLOW)Cleaning build artifacts...$(NC)\n"
	@rm -rf $(BIN_DIR)
	@$(GOCLEAN)

# Build binary
$(BIN_DIR)/$(BINARY_NAME): $(GO_FILES)
	@printf "$(BLUE)Building $(BINARY_NAME) [DEBUG=$(DEBUG)]...$(NC)\n"
	@mkdir -p $(BIN_DIR)
	@$(CGO_FLAGS) $(GOBUILD) $(GCFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME) $(MAIN_PKG)

# Run the application（监听 :5568 gRPC + :8080 HTTP）
run: $(BIN_DIR)/$(BINARY_NAME)
	@printf "$(BLUE)Running $(BINARY_NAME) (gRPC $(GRPC_ADDR) / HTTP $(HTTP_ADDR))...$(NC)\n"
	@$(BIN_DIR)/$(BINARY_NAME)

# Run the application directly without building a binary
dev:
	@printf "$(BLUE)Running via 'go run' (gRPC $(GRPC_ADDR) / HTTP $(HTTP_ADDR))...$(NC)\n"
	@$(GOCMD) run $(MAIN_PKG)

test:
	@printf "$(BLUE)Running tests ...$(NC)\n"
	@$(GOTEST) -v $(PKG_LIST)

fumpt:
	@printf "$(BLUE)Running fumpt ...$(NC)\n"
	@$(GOFUMPT) -w -l $(GO_FILES)

lint:
	@printf "$(BLUE)Running linter ...$(NC)\n"
	@$(GOLINT) run ./...

tidy:
	@printf "$(BLUE)Tidying and verifying module dependencies ...$(NC)\n"
	@# -e: 容忍「依赖的测试依赖」无法解析的报错（如 testify→go-spew/go-difflib 这类
	@#     无 go.mod 的上古包在 module 图裁剪下被 @latest 误判为 "does not contain package"）。
	@#     这些包仅被第三方库的测试使用，本项目并不需要，不影响编译与运行。
	@$(GOMOD) tidy -e
	@$(GOMOD) verify

help:
	@printf "$(BLUE)Available targets:$(NC)\n"
	@printf "  $(GREEN)all (build)$(NC)  : Full build pipeline (clean + tidy + fumpt + lint + compile)\n"
	@printf "  $(GREEN)compile$(NC)      : Quick build without linting (tidy + compile only)\n"
	@printf "  $(GREEN)debug$(NC)        : Build with debug symbols (no optimizations)\n"
	@printf "  $(GREEN)proto$(NC)        : Generate protobuf/gRPC/gateway code (runs generate.sh)\n"
	@printf "  $(GREEN)install-tools$(NC): Install protoc-gen-go / protoc-gen-go-grpc / protoc-gen-grpc-gateway\n"
	@printf "  $(GREEN)run$(NC)          : Build and run the application\n"
	@printf "  $(GREEN)dev$(NC)          : Run directly via 'go run' (no binary)\n"
	@printf "  $(GREEN)test$(NC)         : Run all tests\n"
	@printf "  $(GREEN)fumpt$(NC)        : Format code with gofumpt\n"
	@printf "  $(GREEN)lint$(NC)         : Run golangci-lint for code quality checks\n"
	@printf "  $(GREEN)tidy$(NC)         : Tidy and verify go modules\n"
	@printf "  $(GREEN)clean$(NC)        : Remove binaries and clean build cache\n"
	@printf "  $(GREEN)help$(NC)         : Display this help message\n"
	@printf "\n"
	@printf "$(BLUE)Build modes:$(NC)\n"
	@printf "  Release (default): Optimized + stripped symbols → smaller binary\n"
	@printf "  Debug mode:        Full debug info + no optimizations → for debugging\n"
	@printf "\n"
	@printf "$(BLUE)Examples:$(NC)\n"
	@printf "  make              # Full build in release mode\n"
	@printf "  make proto        # Regenerate code after editing proto/user.proto\n"
	@printf "  make dev          # Run directly without building\n"
	@printf "  make debug        # Build in debug mode\n"
	@printf "  make run          # Build and run\n"

# Debugging
print-%:
	@echo '$*=$($*)'
