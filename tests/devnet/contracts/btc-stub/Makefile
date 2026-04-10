ROOT_DIR 		:= $(abspath .)
BIN_DIR			:= bin

TARGET = contract/main.go
BASEFLAGS = -gc=custom -scheduler=none -panic=trap -no-debug -target=wasm-unknown

TINYJSON		:= tinyjson
TINYJSON_FLAGS 	:= -snake_case -no_std_marshalers
CONTRACTS_DIR	:= contract

TINYGO_IMAGE := tinygo/tinygo:0.39.0
WORKDIR      := /work

TINYGO_CMD = docker run --rm \
    -u $(shell id -u):$(shell id -g) \
    -v $(CURDIR):$(WORKDIR) \
    -w $(WORKDIR) \
    $(TINYGO_IMAGE) \
    tinygo

ifeq ($(USE_DOCKER),0)
    TINYGO_CMD = tinygo
endif

all: build

pull-tinygo:
	docker pull $(TINYGO_IMAGE)

# Regenerate tinyjson marshalers for the params package. The generated
# file (params_tinyjson.go) is checked in, so this target only needs to
# run when params.go changes.
regen-tinyjson:
	@if ! command -v $(TINYJSON) >/dev/null 2>&1; then \
		echo "tinyjson not found, installing..."; \
		go install github.com/CosmWasm/tinyjson/...@latest; \
		echo "tinyjson installed to $$(go env GOPATH)/bin"; \
		echo "ensure $$(go env GOPATH)/bin is on your PATH and re-run"; \
		exit 1; \
	fi
	$(TINYJSON) $(TINYJSON_FLAGS) ./contract/params/params.go

build:
	@mkdir -p $(BIN_DIR)
	@echo "Building btc-stub.wasm ..."
	$(TINYGO_CMD) build $(BASEFLAGS) -o $(BIN_DIR)/build.wasm $(TARGET)

clean:
	rm -rf $(BIN_DIR)/*.wasm
