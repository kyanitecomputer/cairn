# Cairn (ASPEED AST2700 DC-SCM) TamaGo build system.
#
# Produces:
#   bin/cairn.elf   TamaGo ELF for the AST2700 DC-SCM EVB (arm64, linked for DRAM)
#
# Layout: each hardware platform lives under target/<platform>/ and holds its
# entry point (main.go) plus any non-upstream boot assets. The AST2700 DC-SCM
# platform is target/ast2700-dcscm-evb. CA35 bring-up and the DRAM image load
# are handled by the board's BootMCU + the upstream TamaGo board package, so no
# in-tree boot stub is required (unlike vein's FSL91030 flashboot.S).
#
# NOTE: this is the base scaffold. The RUNTIME_TEXT load address below is
# provisional (DRAM base + 64 KB) and must be confirmed against the BootMCU
# payload load address during hardware enablement.

# ---------------------------------------------------------------------------
# Toolchain
# ---------------------------------------------------------------------------
TAMAGO ?= tamago
ifeq ($(shell command -v $(TAMAGO) 2>/dev/null),)
$(error TamaGo compiler '$(TAMAGO)' not found. Set the TAMAGO environment variable to your tamago-go binary)
endif

READELF ?= llvm-readelf

# ---------------------------------------------------------------------------
# Build settings
# ---------------------------------------------------------------------------
GOARCH  = arm64
GOOS    = tamago
GOOSPKG = github.com/usbarmory/tamago

# GOARM64 pins the ARMv8 baseline. The AST2700 CA35 cores are Cortex-A35, which
# implement ARMv8.0-A and do NOT provide FEAT_LSE (large-system atomics). Pin it
# explicitly to v8.0 so the compiler never emits LSE instructions (CAS/LDADD/…),
# which are UNDEFINED on this core and trap synchronously. Do not rely on the
# toolchain default: go1.27 keeps it at v8.0, but a future bump to v8.1+ would
# silently emit unconditional LSE atomics and brick the CA35 payload.
GOARM64 = v8.0

GOENV = GOOS=$(GOOS) GOARCH=$(GOARCH) GOARM64=$(GOARM64) GOOSPKG=$(GOOSPKG) GOTOOLCHAIN=local

# Platform directory: entry point + platform code.
TARGET = target/ast2700-dcscm-evb

# CA35-view DRAM text base. The payload links 64 MB into DRAM
# (RamStart 0x400000000 + 0x4000000), leaving ~960 MB heap between the binary
# end and the stack — the layout proven by the cmd/nats validation.
RUNTIME_TEXT = 0x404000000
ALIGN        = 0x1000

# Build tags. linkcpuinit brings up the CA35 secondary-init path; ast2700dcscm
# selects the DC-SCM board package. Override BOARD_TAG for other silicon.
BOARD_TAG ?= ast2700dcscm
TAGS       = linkcpuinit,$(BOARD_TAG)

# Set VIDEO=1 to compile in the DisplayPort framebuffer console. This enables
# the ast2700video feature and redirects the runtime printk hook onto the
# on-screen console via linkprintk (the board package's own printk must be
# disabled, which linkprintk does).
ifeq ($(VIDEO),1)
TAGS := $(TAGS),ast2700video,linkprintk
endif

# FACETUI=1 (or TAGS_EXTRA=facetui) embeds the facet SPA bundle in the web UI.
# The Dagger UI build populates target/.../webui/build/ before enabling this.
ifeq ($(FACETUI),1)
TAGS := $(TAGS),facetui
endif
ifneq ($(TAGS_EXTRA),)
TAGS := $(TAGS),$(TAGS_EXTRA)
endif

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------
BIN_DIR = bin
ELF     = $(BIN_DIR)/cairn.elf

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
.PHONY: all clean build flashdiag flashdiag-dma test imgtools help

all: build

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

# Build the TamaGo ELF for the AST2700 DC-SCM EVB. DRAM is prepared by the
# BootMCU, so this is linked for the CA35 DRAM view.
build: $(BIN_DIR)
	@echo "Building Cairn ELF (AST2700 DC-SCM EVB)..."
	$(GOENV) $(TAMAGO) build \
		-trimpath \
		-tags $(TAGS) \
		-ldflags "-T $(RUNTIME_TEXT) -R $(ALIGN)" \
		-o $(ELF) \
		./$(TARGET)
	@echo "Built: $(ELF)"
	@$(READELF) -h $(ELF) | grep -i "entry point"

# Read-only FMC/SPI-NOR hardware diagnostic payload. Same link layout as the
# normal payload, but the `flashdiag` tag makes it run the probe suite and idle
# instead of booting. Stitch it into a flashable image exactly like the normal
# payload, e.g.:
#
#   dagger call image --build-tags "linkcpuinit,ast2700dcscm,flashdiag" \
#       --silicon a1 ... export --path .
#
# Add `flashdiagdma` to the tag list to also run the (opt-in) DMA-scaling probe.
FLASHDIAG_ELF  = $(BIN_DIR)/cairn-flashdiag.elf
FLASHDIAG_TAGS = $(BOARD_TAG),linkcpuinit,flashdiag$(if $(TAGS_EXTRA),$(comma)$(TAGS_EXTRA))
comma := ,
flashdiag: $(BIN_DIR)
	@echo "Building Cairn flash-diagnostic ELF (tags: $(FLASHDIAG_TAGS))..."
	$(GOENV) $(TAMAGO) build \
		-trimpath \
		-tags $(FLASHDIAG_TAGS) \
		-ldflags "-T $(RUNTIME_TEXT) -R $(ALIGN)" \
		-o $(FLASHDIAG_ELF) \
		./$(TARGET)
	@echo "Built: $(FLASHDIAG_ELF)"
	@$(READELF) -h $(FLASHDIAG_ELF) | grep -i "entry point"

flashdiag-dma: TAGS_EXTRA := flashdiagdma
flashdiag-dma: flashdiag

# Host unit tests (Linux userspace).
test:
	$(TAMAGO) test -tags user_linux ./... || true

# Build the host image-stitching tool. The full SPI flash image (BootMCU + this
# payload + bmc-pb prebuilts) is produced by `dagger call image`, which also
# builds the BootMCU Rust firmware; imgtools is the stitcher it invokes.
imgtools: $(BIN_DIR)
	GOWORK=off $(TAMAGO) build -C tools/imgtools -o $(abspath $(BIN_DIR))/imgtools .
	@echo "Built: $(BIN_DIR)/imgtools"

clean:
	@rm -rf $(BIN_DIR)
	@echo "Clean complete"

help:
	@echo "Cairn (ASPEED AST2700 DC-SCM) TamaGo - Build System"
	@echo ""
	@echo "Hardware: ASPEED AST2700 (dual Cortex-A35, arm64)"
	@echo "DRAM:     1 GB, CA35 view at 0x400000000"
	@echo ""
	@echo "Targets:"
	@echo "  build     Build TamaGo ELF for the AST2700 DC-SCM EVB (default)"
	@echo "  test      Run host tests (user_linux tag)"
	@echo "  imgtools  Build the host image-stitching tool"
	@echo "  clean     Remove build artifacts"
	@echo ""
	@echo "Full SPI flash image: dagger call image ... --silicon {a1|a2}"
	@echo ""
	@echo "Environment variables:"
	@echo "  TAMAGO     Path to tamago-go binary (default: $(TAMAGO))"
	@echo "  READELF    LLVM readelf (default: llvm-readelf)"
	@echo "  BOARD_TAG  Board build tag (default: ast2700dcscm)"
