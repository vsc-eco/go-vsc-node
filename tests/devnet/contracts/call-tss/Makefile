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
    $(GOWORK_EXTRA_MOUNTS) \
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

build:
	@echo "Building $@ ..."
	$(TINYGO_CMD) build $(BASEFLAGS) -o $(BIN_DIR)/$@.wasm $(TARGET)

clean:
	rm -rf $(BIN_DIR)/*.wasm