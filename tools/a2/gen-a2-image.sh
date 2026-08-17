#!/usr/bin/env bash
#
# AST2700-A2 end-to-end image pipeline for cairn.
#
# A2 boots via a Caliptra "FLSH" flash container carrying the Caliptra FW, a
# signed SoC auth-manifest (ATMN), the MCU runtime (BootMCU FMC) and the SoC
# images (DRAM/DP prebuilts + the cairn CA35 payload). Two tools are involved:
#
#   1. cptra_imgtool (vendor, Rust)  -> generates + signs the ATMN SoC manifest.
#      The LMS/ECDSA-P384 signing is not reasonably portable to Go, so we drive
#      the vendor tool for this step (build: see cptra_imgtool/README.md).
#   2. imgtools flsh-image (ours)    -> stitches the FLSH container. This is a
#      byte-for-byte port of the caliptra-mcu-sw builder, verified against the
#      official ASPEED A2 image.
#
# The AST2700 dev board is unprovisioned, so cptra_imgtool's default dev keys
# are used (unsigned/dev flow). The full signed flow (real owner keys, OTP
# provisioning) is a later step.
#
# Usage:
#   CPTRA_IMGTOOL=../cptra_imgtool PREBUILT=/tmp/a2img/ast2700-irot \
#     tools/a2/gen-a2-image.sh
#
# Env:
#   TAMAGO         tamago-go binary (for the cairn payload build)
#   CPTRA_IMGTOOL  path to the built cptra_imgtool checkout
#   PREBUILT       dir with caliptra-fw.bin, ast2700-mcu-runtime.bin and the
#                  ddr*/dp_fw prebuilts (e.g. the extracted official A2 tarball)
#   KEYDIR         cptra_imgtool key dir (default: <CPTRA_IMGTOOL>/key/ast2700-default)
#   OUT            output dir (default: out)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CAIRN="$(cd "$HERE/../.." && pwd)"

# DIAG=1 builds the read-only FMC/SPI-NOR hardware diagnostic payload
# (`flashdiag` tag) instead of the normal management-plane payload, and writes
# the image to cairn_ast2700_a2_flashdiag.bin. DIAGDMA=1 additionally enables
# the opt-in DMA-scaling probe (`flashdiagdma`); DIAGWRITE=1 enables the
# destructive write/erase validation (`flashdiagwrite`, operates only on a
# scratch sector above the boot image). Everything else in the A2 pipeline
# (BootMCU, prebuilts, SoC manifest regeneration, FLSH stitch) is identical, so
# the diagnostic boots exactly like the real payload.
DIAG="${DIAG:-0}"
DIAGDMA="${DIAGDMA:-0}"
DIAGWRITE="${DIAGWRITE:-0}"
if [ "$DIAG" = "1" ]; then
	IMAGE_NAME="cairn_ast2700_a2_flashdiag.bin"
	PAYLOAD_ELF="$CAIRN/bin/cairn-flashdiag.elf"
else
	IMAGE_NAME="cairn_ast2700_a2.bin"
	PAYLOAD_ELF="$CAIRN/bin/cairn.elf"
fi

TAMAGO="${TAMAGO:-tamago}"
CPTRA_IMGTOOL="${CPTRA_IMGTOOL:-$CAIRN/../cptra_imgtool}"
PREBUILT="${PREBUILT:?set PREBUILT to a dir with caliptra-fw/mcu-runtime/prebuilts}"
# ast2700-default supplies the ECC+LMS dev keys; MLDSA_KEYDIR supplies the MLDSA
# dev keys this tool version requires (present-but-unused under LMS PQC).
KEYSRC="${KEYSRC:-$CPTRA_IMGTOOL/key/ast2700-default}"
MLDSA_KEYSRC="${MLDSA_KEYSRC:-$CPTRA_IMGTOOL/key/ast1040a0-default}"
OUT="${OUT:-$CAIRN/out}"
STAGE="$OUT/a2-stage"
KEYDIR="$STAGE/keys"

IMGTOOLS="$CAIRN/bin/imgtools"
# The SoC-manifest generator is the `create-auth-man-2x` subcommand of the
# cptra-imgtool binary. Prefer the prebuilt release binary so we don't trigger a
# rustup toolchain re-sync (the pinned channel + llvm-tools components can fail
# to install in some environments); fall back to `cargo run` if it's absent.
MANIFEST_TOOL="$CPTRA_IMGTOOL/target/release/cptra-imgtool"

mkdir -p "$OUT" "$STAGE"

# Remove any previous image up front. With `set -e`, a failure in an earlier
# step (e.g. a missing prebuilt) aborts before the stitch step, and without this
# the old image would remain in place looking freshly built — a flash programmer
# then re-flashes stale firmware. Deleting it makes such failures unmistakable.
rm -f "$OUT/$IMAGE_NAME"

echo "==> 1/4 build cairn CA35 payload"
# Forward VIDEO=1 (framebuffer console) and FACETUI=1 (embed the facet SPA).
CAIRN_TAGS=""
if [ -n "${FACETUI:-}" ]; then
	CAIRN_TAGS="facetui"
fi
if [ "$DIAG" = "1" ]; then
	DIAG_EXTRA="$CAIRN_TAGS"
	[ "$DIAGDMA" = "1" ] && DIAG_EXTRA="${DIAG_EXTRA:+$DIAG_EXTRA,}flashdiagdma"
	[ "$DIAGWRITE" = "1" ] && DIAG_EXTRA="${DIAG_EXTRA:+$DIAG_EXTRA,}flashdiagwrite"
	echo "    DIAG build: make flashdiag TAGS_EXTRA=$DIAG_EXTRA"
	( cd "$CAIRN" && TAMAGO="$TAMAGO" TAGS_EXTRA="$DIAG_EXTRA" make flashdiag )
else
	( cd "$CAIRN" && TAMAGO="$TAMAGO" VIDEO="${VIDEO:-}" TAGS_EXTRA="$CAIRN_TAGS" make build )
fi
llvm-objcopy -O binary "$PAYLOAD_ELF" "$STAGE/cairn.payload.bin"

echo "==> 2/4 build imgtools (host)"
( cd "$CAIRN/tools/imgtools" && GOWORK=off "$TAMAGO" build -o "$IMGTOOLS" . )

# Prefix the CA35 payload with the 16-byte boot header carrying the entry
# offset, so the BootMCU jumps to _rt0 without a hardcoded constant. cairn.raw.bin
# (header || payload) is what both the SoC manifest and the FLSH container use.
#
# A35_COMPRESS=0 (default) stores the CA35 payload verbatim — the reliable path.
# A35_COMPRESS=1 m77rip-compresses it (BootMCU auto-detects the m77 header magic
# and decompresses XIP->DRAM at boot). Compression currently trips the
# BootROM/Caliptra secure-boot manifest stage (the flashed compressed bytes
# don't match the runtime image Caliptra wants to authorize), so it is opt-in
# pending the Caliptra manifest work (see docs/bootmcu-runtime-plan.md WS5).
A35_COMPRESS="${A35_COMPRESS:-0}"
if [ "$A35_COMPRESS" = "1" ]; then
	echo "    compressing CA35 payload with m77rip"
	M77_COMPRESS="$CAIRN/bin/m77rip-compress"
	( cd "$CAIRN/tools/m77rip-compress" && cargo build --release -q \
		&& cp target/release/m77rip-compress "$M77_COMPRESS" )
	"$M77_COMPRESS" "$STAGE/cairn.payload.bin" "$STAGE/cairn.payload.m77"
	"$IMGTOOLS" a35-header \
		--elf "$PAYLOAD_ELF" \
		--in "$STAGE/cairn.payload.m77" \
		--compressed \
		--out "$STAGE/cairn.raw.bin"
else
	"$IMGTOOLS" a35-header \
		--elf "$PAYLOAD_ELF" \
		--in "$STAGE/cairn.payload.bin" \
		--out "$STAGE/cairn.raw.bin"
fi

echo "==> 3/4 stage prebuilts + generate SoC manifest (cptra_imgtool)"
for f in caliptra-fw.bin \
         ddr4_pmu_train_imem.bin ddr4_pmu_train_dmem.bin \
         ddr4_2d_pmu_train_imem.bin ddr4_2d_pmu_train_dmem.bin \
         ddr5_pmu_train_imem.bin ddr5_pmu_train_dmem.bin dp_fw.bin; do
	cp "$PREBUILT/$f" "$STAGE/$f"
done

# The MCU runtime (BootMCU FMC, FLSH id 3). Defaults to the vendor binary; set
# MCU_RUNTIME_BIN to our own aspeed-mcu-runtime FMC (built for A2, linked at
# 0x14B80000) to run the cairn Rust BootMCU instead of vendor Zephyr.
MCU_RUNTIME_BIN="${MCU_RUNTIME_BIN:-$PREBUILT/ast2700-mcu-runtime.bin}"
cp "$MCU_RUNTIME_BIN" "$STAGE/ast2700-mcu-runtime.bin"
echo "    MCU runtime: $MCU_RUNTIME_BIN"

if [ ! -x "$MANIFEST_TOOL" ]; then
	echo "    manifest binary not prebuilt; will use 'cargo run' (may sync rustup)"
fi

# Assemble a combined key dir: ECC+LMS dev keys + MLDSA dev keys (see config).
rm -rf "$KEYDIR" && mkdir -p "$KEYDIR"
cp "$KEYSRC"/*.pem "$KEYDIR/"
for k in vnd-fw-mldsa-pub-key-0 vnd-fw-mldsa-priv-key-0 \
         vnd-man-mldsa-pub-key vnd-man-mldsa-priv-key \
         own-fw-mldsa-pub-key own-fw-mldsa-priv-key \
         own-man-mldsa-pub-key own-man-mldsa-priv-key; do
	cp "$MLDSA_KEYSRC/$k.bin" "$KEYDIR/"
done

if [ -x "$MANIFEST_TOOL" ]; then
	"$MANIFEST_TOOL" create-auth-man-2x \
		--cfg "$HERE/cairn-a2-manifest.toml" \
		--key-dir "$KEYDIR" \
		--prebuilt-dir "$STAGE" \
		--pqc-key-type 3 \
		--man "$STAGE/cairn-soc-manifest.bin"
else
	( cd "$CPTRA_IMGTOOL" && cargo run --release -- create-auth-man-2x \
		--cfg "$HERE/cairn-a2-manifest.toml" \
		--key-dir "$KEYDIR" \
		--prebuilt-dir "$STAGE" \
		--pqc-key-type 3 \
		--man "$STAGE/cairn-soc-manifest.bin" )
fi

echo "==> 4/4 stitch FLSH container (imgtools)"
"$IMGTOOLS" flsh-image \
	--caliptra "$STAGE/caliptra-fw.bin" \
	--soc-manifest "$STAGE/cairn-soc-manifest.bin" \
	--mcu-runtime "$STAGE/ast2700-mcu-runtime.bin" \
	--soc-image "$STAGE/ddr4_pmu_train_imem.bin" \
	--soc-image "$STAGE/ddr4_pmu_train_dmem.bin" \
	--soc-image "$STAGE/ddr4_2d_pmu_train_imem.bin" \
	--soc-image "$STAGE/ddr4_2d_pmu_train_dmem.bin" \
	--soc-image "$STAGE/ddr5_pmu_train_imem.bin" \
	--soc-image "$STAGE/ddr5_pmu_train_dmem.bin" \
	--soc-image "$STAGE/dp_fw.bin" \
	--soc-image "$STAGE/cairn.raw.bin" \
	--output "$OUT/$IMAGE_NAME"

echo "Done: $OUT/$IMAGE_NAME"
