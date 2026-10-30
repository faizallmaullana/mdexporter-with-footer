GO ?= go
BUILD_DIR ?= build
BINARY := $(BUILD_DIR)/mdexport

.PHONY: all build install clean

all: build

build: $(BINARY)

$(BINARY): main.go go.mod
	mkdir -p $(BUILD_DIR)
	$(GO) build -buildvcs=false -o $(BINARY) .

install: $(BINARY) install.sh public/footer.png
	./install.sh

clean:
	rm -rf $(BUILD_DIR)
