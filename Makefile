# Build directory
BUILD_DIR := ./build

# Git commit for version info
GIT_COMMIT := $(shell git rev-parse HEAD)

# Build flags
BUILD_FLAGS := -buildvcs=false
LDFLAGS := -X vsc-node/modules/announcements.GitCommit=$(GIT_COMMIT)

# Installation directory (default to /usr/local/bin)
PREFIX ?= /usr/local
INSTALL_DIR := $(PREFIX)/bin

# Targets
.PHONY: all clean install magid contract-deployer genesis-elector devnet-setup mapping-bot

all: magid contract-deployer genesis-elector devnet-setup mapping-bot

magid: $(BUILD_DIR)/magid
	@echo "Built magid"

contract-deployer: $(BUILD_DIR)/contract-deployer
	@echo "Built contract-deployer"

genesis-elector: $(BUILD_DIR)/genesis-elector
	@echo "Built genesis-elector"

devnet-setup: $(BUILD_DIR)/devnet-setup
	@echo "Built devnet-setup"

mapping-bot: $(BUILD_DIR)/mapping-bot
	@echo "Built mapping-bot"

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

$(BUILD_DIR)/magid: $(BUILD_DIR)
	go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $@ vsc-node/cmd/vsc-node

$(BUILD_DIR)/contract-deployer: $(BUILD_DIR)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/contract-deployer

$(BUILD_DIR)/genesis-elector: $(BUILD_DIR)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/genesis-elector

$(BUILD_DIR)/devnet-setup: $(BUILD_DIR)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/devnet-setup

$(BUILD_DIR)/mapping-bot: $(BUILD_DIR)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/mapping-bot

install: all
	mkdir -p $(INSTALL_DIR)
	cp $(BUILD_DIR)/magid $(INSTALL_DIR)/
	cp $(BUILD_DIR)/contract-deployer $(INSTALL_DIR)/
	cp $(BUILD_DIR)/genesis-elector $(INSTALL_DIR)/
	cp $(BUILD_DIR)/devnet-setup $(INSTALL_DIR)/
	cp $(BUILD_DIR)/mapping-bot $(INSTALL_DIR)/

clean:
	rm -rf $(BUILD_DIR)
