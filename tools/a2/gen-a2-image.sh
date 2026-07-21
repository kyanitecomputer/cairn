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

TAMAGO="${TAMAGO:-/home/mdr164/private/tamago/tamago-go/bin/go}"
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
MANIFEST_TOOL="$CPTRA_IMGTOOL/target/release/caliptra-auth-manifest-app-2x"

mkdir -p "$OUT" "$STAGE"

# Remove any previous image up front. With `set -e`, a failure in an earlier
# step (e.g. a missing prebuilt) aborts before the stitch step, and without this
# the old image would remain in place looking freshly built — a flash programmer
# then re-flashes stale firmware. Deleting it makes such failures unmistakable.
rm -f "$OUT/cairn_ast2700_a2.bin"

echo "==> 1/4 build cairn CA35 payload"
( cd "$CAIRN" && TAMAGO="$TAMAGO" make build )
llvm-objcopy -O binary "$CAIRN/bin/cairn.elf" "$STAGE/cairn.raw.bin"

echo "==> 2/4 build imgtools (host)"
( cd "$CAIRN/tools/imgtools" && GOWORK=off "$TAMAGO" build -o "$IMGTOOLS" . )

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
	echo "ERROR: manifest tool not built: $MANIFEST_TOOL" >&2
	echo "Build it: (cd $CPTRA_IMGTOOL && cargo build --release -p caliptra-auth-manifest-app-2x)" >&2
	exit 1
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

( cd "$CPTRA_IMGTOOL" && cargo run --release -- create-auth-man-2x \
	--cfg "$HERE/cairn-a2-manifest.toml" \
	--key-dir "$KEYDIR" \
	--prebuilt-dir "$STAGE" \
	--pqc-key-type 3 \
	--man "$STAGE/cairn-soc-manifest.bin" )

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
	--output "$OUT/cairn_ast2700_a2.bin"

echo "Done: $OUT/cairn_ast2700_a2.bin"
