GO ?= go
LZ4 ?= lz4
OUT ?= .out

UROOT := github.com/u-root/u-root
INIT := $(OUT)/init
INITRAMFS := $(OUT)/initramfs.cpio.lz4
BUILD := $(abspath $(OUT)/.build)

.PHONY: all clean
all: $(INITRAMFS)

$(INIT): $(wildcard cmd/init/*.go) go.mod go.sum Makefile
	mkdir -p "$(@D)"
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -mod=readonly -trimpath \
		-ldflags='-s -w' -o "$@" ./cmd/init

$(INITRAMFS): $(INIT) go.mod go.sum Makefile
	@set -eu; \
	mkdir -p "$(BUILD)"; \
	trap 'rm -rf "$(BUILD)"' EXIT; \
	CGO_ENABLED=0 $(GO) build -mod=mod -o "$(BUILD)/u-root" $(UROOT); \
	uroot_dir="$$($(GO) list -mod=mod -f '{{.Dir}}' $(UROOT))"; \
	cd "$$uroot_dir"; \
	commands="$$(GOOS=linux GOARCH=arm64 $(GO) list -mod=mod -e \
		-f '{{if and (eq .Name "main") (gt (len .GoFiles) 0)}}{{.ImportPath}}{{end}}' \
		./cmds/*/*)"; \
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOFLAGS=-mod=mod UROOT_SOURCE="$$uroot_dir" \
		"$(BUILD)/u-root" -o "$(abspath $(BUILD)/initramfs.cpio)" \
		-initcmd=/bbin/uinit -uinitcmd="" -defaultsh=gosh -skip-ldd \
		-files "$(abspath $(INIT)):bbin/uinit" \
		$$commands; \
	$(LZ4) -l -12 -c "$(BUILD)/initramfs.cpio" > "$(abspath $(INITRAMFS))"

clean:
	$(RM) "$(INIT)" "$(INITRAMFS)"
	rm -rf "$(BUILD)"
