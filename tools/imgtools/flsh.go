package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AST2700-A2 "FLSH" flash image container.
//
// A2 replaces the A1 ASTH secure-boot header with a top-level flash container
// that the MCU ROM parses to locate the Caliptra firmware, the SoC (auth)
// manifest, the MCU runtime (BootMCU FMC), and any additional SoC images. The
// ROM authorizes each image through Caliptra (SET_AUTH_MANIFEST /
// AUTHORIZE_AND_STASH) using the digests carried in the SoC manifest.
//
// This is a byte-for-byte port of caliptra-sw hw-model/src/flash_image.rs
// (build_flash_image_bytes) — the format the MCU ROM's flash-boot path expects —
// extended with additional SoC images the way the vendor cptra_imgtool does via
// `xtask flash-image create --soc-images`.
//
// Layout:
//
//	FlashHeader (16B)
//	ImageHeader (20B) × image_count
//	image data, each padded to 256 bytes, in header order
//
// All integers are little-endian. The checksum is the two's-complement of the
// byte sum (0 - Σ bytes), computed over each structure excluding its own
// trailing checksum field, and over each image's (padded) data.

const (
	// Canonical MCU-ROM image identifiers (caliptra-sw flash_image.rs).
	flshIDCaliptraFmcRt = 0x0000_0000
	flshIDSocManifest   = 0x0000_0001
	flshIDMcuRt         = 0x0000_0002

	flshMagic          = "FLSH"
	flshHeaderVersion  = 0x0001
	flshHeaderSize     = 16 // magic(4)+version(2)+count(2)+headers_off(4)+checksum(4)
	flshImageHdrSize   = 20 // id(4)+offset(4)+size(4)+img_cksum(4)+hdr_cksum(4)
	flshImageAlignment = 256
)

// flshImage is one image to place in the container.
type flshImage struct {
	id   uint32
	data []byte
}

// flshChecksum returns the two's-complement of the byte sum of data.
func flshChecksum(data []byte) uint32 {
	var sum uint32
	for _, b := range data {
		sum += uint32(b)
	}
	return -sum
}

func flshPadTo256(data []byte) []byte {
	n := (len(data) + flshImageAlignment - 1) &^ (flshImageAlignment - 1)
	if n == len(data) {
		out := make([]byte, len(data))
		copy(out, data)
		return out
	}
	out := make([]byte, n)
	copy(out, data)
	return out
}

// buildFlashImage assembles the FLSH container from the given images (in order).
func buildFlashImage(images []flshImage) []byte {
	if len(images) == 0 {
		return nil
	}

	padded := make([][]byte, len(images))
	for i, img := range images {
		padded[i] = flshPadTo256(img.data)
	}

	dataStart := flshHeaderSize + flshImageHdrSize*len(images)
	offset := uint32(dataStart)

	// Image headers.
	hdrs := make([]byte, 0, flshImageHdrSize*len(images))
	for i, img := range images {
		var h [flshImageHdrSize]byte
		binary.LittleEndian.PutUint32(h[0:4], img.id)
		binary.LittleEndian.PutUint32(h[4:8], offset)
		binary.LittleEndian.PutUint32(h[8:12], uint32(len(padded[i])))
		binary.LittleEndian.PutUint32(h[12:16], flshChecksum(padded[i]))
		// image_header_checksum covers all fields except itself (first 16 bytes).
		binary.LittleEndian.PutUint32(h[16:20], flshChecksum(h[:16]))
		hdrs = append(hdrs, h[:]...)
		offset += uint32(len(padded[i]))
	}

	// Flash header.
	var fh [flshHeaderSize]byte
	copy(fh[0:4], flshMagic)
	binary.LittleEndian.PutUint16(fh[4:6], flshHeaderVersion)
	binary.LittleEndian.PutUint16(fh[6:8], uint16(len(images)))
	binary.LittleEndian.PutUint32(fh[8:12], uint32(flshHeaderSize))
	// header_checksum covers all fields except itself (first 12 bytes).
	binary.LittleEndian.PutUint32(fh[12:16], flshChecksum(fh[:12]))

	out := make([]byte, 0, dataStart)
	out = append(out, fh[:]...)
	out = append(out, hdrs...)
	for _, p := range padded {
		out = append(out, p...)
	}
	return out
}

func cmdFlshImage(args []string) error {
	fs := flag.NewFlagSet("flsh-image", flag.ExitOnError)
	var caliptra, manifest, mcu, output string
	var socImages repeatFlag
	fs.StringVar(&caliptra, "caliptra", "", "Caliptra FW image (id 0)")
	fs.StringVar(&manifest, "soc-manifest", "", "SoC (auth) manifest (id 1)")
	fs.StringVar(&mcu, "mcu-runtime", "", "MCU runtime / BootMCU FMC (id 2)")
	fs.Var(&socImages, "soc-image", "Additional SoC image ID:FILE (repeatable)")
	fs.StringVar(&output, "output", "", "Output FLSH image file")
	fs.StringVar(&output, "o", "", "Output FLSH image file")
	fs.Parse(args)

	if output == "" {
		return fmt.Errorf("--output is required")
	}

	var images []flshImage
	add := func(id uint32, path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		images = append(images, flshImage{id: id, data: data})
		return nil
	}

	if caliptra != "" {
		if err := add(flshIDCaliptraFmcRt, caliptra); err != nil {
			return err
		}
	}
	if manifest != "" {
		if err := add(flshIDSocManifest, manifest); err != nil {
			return err
		}
	}
	if mcu != "" {
		if err := add(flshIDMcuRt, mcu); err != nil {
			return err
		}
	}
	for _, spec := range socImages {
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("--soc-image must be ID:FILE, got: %s", spec)
		}
		id, err := strconv.ParseUint(parts[0], 0, 32)
		if err != nil {
			return fmt.Errorf("parse soc-image id %q: %w", parts[0], err)
		}
		if err := add(uint32(id), parts[1]); err != nil {
			return err
		}
	}

	if len(images) == 0 {
		return fmt.Errorf("no images: supply --caliptra/--soc-manifest/--mcu-runtime and/or --soc-image")
	}

	img := buildFlashImage(images)
	if err := os.WriteFile(output, img, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}

	fmt.Println("=== AST2700-A2 FLSH image ===")
	off := flshHeaderSize + flshImageHdrSize*len(images)
	for _, im := range images {
		psize := len(flshPadTo256(im.data))
		fmt.Printf("  image id=%#010x offset=%#010x size=%#010x (%d raw)\n",
			im.id, off, psize, len(im.data))
		off += psize
	}
	fmt.Printf("  image_count=%d total=%d bytes\n", len(images), len(img))
	fmt.Printf("  Output: %s\n", output)
	return nil
}
