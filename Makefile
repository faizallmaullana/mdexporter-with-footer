GO ?= go
BUILD_DIR ?= build
BINARY := $(BUILD_DIR)/mdexport

.PHONY: all build install clean

all: build

build: $(BINARY)

$(BINARY): main.go go.mod
	mkdir -p $(BUILD_DIR)
	$(GO) build -buildvcs=false -o $(BINARY) .

# install does not build: under sudo, Go is usually missing from PATH and
# building as root would leave root-owned files in $(BUILD_DIR).
# Run `make build` as a normal user first, then `sudo make install`.
install: install.sh public/footer.png
	@if [ ! -f $(BINARY) ]; then \
		echo "error: $(BINARY) not found; run 'make build' first (without sudo)" >&2; exit 1; \
	fi
	@for f in main.go go.mod; do \
		if [ $$f -nt $(BINARY) ]; then \
			echo "error: $(BINARY) is older than $$f; run 'make build' first (without sudo)" >&2; exit 1; \
		fi; \
	done
	./install.sh

clean:
	rm -rf $(BUILD_DIR)
