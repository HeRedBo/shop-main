# 变量定义
APP_NAME := shop
BUILD_DIR := build
GO := go
GOFLAGS := -ldflags="-s -w"

# 交叉编译环境变量（默认当前平台）
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

.PHONY: all build server cli cron worker clean help run-server run-cli run-cron run-worker

# 默认目标：编译所有
all: server cli cron worker

# 编译 HTTP 服务
server:
	@echo "==> 编译 HTTP 服务..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-server ./main.go
	@echo "==> 输出: $(BUILD_DIR)/$(APP_NAME)-server"

# 编译 CLI 工具
cli:
	@echo "==> 编译 CLI 工具..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-cli ./cmd/cli/
	@echo "==> 输出: $(BUILD_DIR)/$(APP_NAME)-cli"

# 编译定时任务服务
cron:
	@echo "==> 编译定时任务服务..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-cron ./cmd/cron/
	@echo "==> 输出: $(BUILD_DIR)/$(APP_NAME)-cron"

# 编译 Worker 服务
worker:
	@echo "==> 编译 Worker 服务..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(APP_NAME)-worker ./cmd/worker/
	@echo "==> 输出: $(BUILD_DIR)/$(APP_NAME)-worker"

# 编译所有
build: all

# 交叉编译 Linux 版本
build-linux:
	@echo "==> 交叉编译 Linux 版本..."
	@$(MAKE) all GOOS=linux GOARCH=amd64
	@echo "==> Linux 二进制文件已输出到 $(BUILD_DIR)/"

# 运行 HTTP 服务
run-server: server
	@echo "==> 运行 $(APP_NAME)-server..."
	@./$(BUILD_DIR)/$(APP_NAME)-server

# 运行 CLI 工具
run-cli: cli
	@echo "==> 运行 $(APP_NAME)-cli..."
	@./$(BUILD_DIR)/$(APP_NAME)-cli $(RUN_ARGS)

# 运行 Worker 服务
run-worker: worker
	@echo "==> 运行 $(APP_NAME)-worker..."
	@./$(BUILD_DIR)/$(APP_NAME)-worker

# 运行定时任务服务
run-cron: cron
	@echo "==> 运行 $(APP_NAME)-cron..."
	@./$(BUILD_DIR)/$(APP_NAME)-cron

# 清理构建产物
clean:
	@echo "==> 清理构建产物..."
	@rm -rf $(BUILD_DIR)
	@echo "==> 清理完成"

# 帮助信息
help:
	@echo "可用命令："
	@echo "  make build        - 编译所有服务（server + cli + cron + worker）"
	@echo "  make server       - 仅编译 HTTP 服务"
	@echo "  make cli          - 仅编译 CLI 工具"
	@echo "  make cron         - 仅编译定时任务服务"
	@echo "  make worker       - 仅编译 Worker 服务"
	@echo "  make build-linux  - 交叉编译 Linux 版本"
	@echo "  make run-server   - 运行 HTTP 服务"
	@echo "  make run-cli      - 运行 CLI 工具（可加 RUN_ARGS=\"--help\"）"
	@echo "  make run-cron     - 运行定时任务服务"
	@echo "  make run-worker   - 运行 Worker 服务"
	@echo "  make clean        - 清理构建产物"
	@echo "  make help         - 显示帮助信息"
