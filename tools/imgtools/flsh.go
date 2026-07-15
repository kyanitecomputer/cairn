package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"hash/crc32"
	"os"
)

// AST2700-A2 "FLSH" flash image container.
//
// A2 replaces the A1 ASTH secure-boot header with a top-level "FLSH" flash
// container that the MCU ROM parses to locate the Caliptra firmware, the SoC
// (auth) manifest, the MCU runtime (BootMCU FMC), and any additional SoC images;
// the ROM then authorizes each image through Caliptra (SET_AUTH_MANIFEST /
// AUTHORIZE_AND_STASH) using the digests carried in the SoC manifest. Our A1
// ASTH images therefore do not boot on A2.
//
// This is a byte-for-byte port of the caliptra-mcu-sw flash-image builder
// (builder/src/flash_image.rs) at the AST2700 A1/A2 pinned revision
// 2b7837402328ab611968d40243075082469df7ae, verified against the official
// ASPEED A2 flash image (ast2700-manifest-flash.bin): both CRC-32 checksums and
// the image-info table reproduce exactly.
//
// Layout (all little-endian except the ASCII magic):
//
//	Header        magic[4]="FLSH" (0x464C5348) | version u16=1 | image_count u16
//	Checksums     header_crc32 u32 | payload_crc32 u32
//	ImageInfo[n]  identifier u32 | image_offset u32 | size u32       (12 bytes each)
//	Images        raw data, each padded to a 4-byte boundary, in ImageInfo order
//
//   - header_crc32  = CRC-32/IEEE over the 8-byte Header.
//   - payload_crc32 = CRC-32/IEEE over the ImageInfo table + all (padded) images.
//   - image_offset  is absolute from byte 0 of the Header.
//   - size          is the padded (4-byte-aligned) length.

const (
	flshMagic         = "FLSH" // stored big-endian: 0x464C5348 == bytes 'F','L','S','H'
	flshHeaderVersion = 0x0001

	flshHeaderSize = 8  // magic(4) + version(2) + image_count(2)
	flshCksumSize  = 8  // header_crc32(4) + payload_crc32(4)
	flshInfoSize   = 12 // identifier(4) + image_offset(4) + size(4)

	// Image identifiers (caliptra-mcu-sw builder).
	flshIDCaliptraFmcRt = 0x0000_0001
	flshIDSocManifest   = 0x0000_0002
	flshIDMcuRt         = 0x0000_0003
	flshIDSocImagesBase = 0x0000_1000 // additional SoC images, incrementing
)

// flshImage is one image to place in the container (data already read).
type flshImage struct {
	id   uint32
	data []byte
}

func flshCrc32(data []byte) uint32 { return crc32.ChecksumIEEE(data) }

// flshPad4 returns data padded with zeros to a 4-byte boundary.
func flshPad4(data []byte) []byte {
	n := (len(data) + 3) &^ 3
	if n == len(data) {
		return data
	}
	out := make([]byte, n)
	copy(out, data)
	return out
}

// buildFlashImage assembles the FLSH container from the given images (in order).
// Each image's data is padded to 4 bytes; the reported size is the padded size.
func buildFlashImage(images []flshImage) []byte {
	if len(images) == 0 {
		return nil
	}

	padded := make([][]byte, len(images))
	for i, img := range images {
		padded[i] = flshPad4(img.data)
	}

	// Image-info table. image_offset is absolute from byte 0.
	infoBase := flshHeaderSize + flshCksumSize + flshInfoSize*len(images)
	offset := uint32(infoBase)
	info := make([]byte, 0, flshInfoSize*len(images))
	for i, img := range images {
		var e [flshInfoSize]byte
		binary.LittleEndian.PutUint32(e[0:4], img.id)
		binary.LittleEndian.PutUint32(e[4:8], offset)
		binary.LittleEndian.PutUint32(e[8:12], uint32(len(padded[i])))
		info = append(info, e[:]...)
		offset += uint32(len(padded[i]))
	}

	// Header.
	var hdr [flshHeaderSize]byte
	copy(hdr[0:4], flshMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], flshHeaderVersion)
	binary.LittleEndian.PutUint16(hdr[6:8], uint16(len(images)))

	// Payload CRC covers the image-info table followed by all image data.
	payloadCrc := crc32.NewIEEE()
	payloadCrc.Write(info)
	for _, p := range padded {
		payloadCrc.Write(p)
	}

	var cks [flshCksumSize]byte
	binary.LittleEndian.PutUint32(cks[0:4], flshCrc32(hdr[:]))
	binary.LittleEndian.PutUint32(cks[4:8], payloadCrc.Sum32())

	out := make([]byte, 0, offset)
	out = append(out, hdr[:]...)
	out = append(out, cks[:]...)
	out = append(out, info...)
	for _, p := range padded {
		out = append(out, p...)
	}
	return out
}

func cmdFlshImage(args []string) error {
	fs := flag.NewFlagSet("flsh-image", flag.ExitOnError)
	var caliptra, manifest, mcu, output string
	var socImages repeatFlag
	fs.StringVar(&caliptra, "caliptra", "", "Caliptra FW image (id 0x0001)")
	fs.StringVar(&manifest, "soc-manifest", "", "SoC (auth) manifest (id 0x0002)")
	fs.StringVar(&mcu, "mcu-runtime", "", "MCU runtime / BootMCU FMC (id 0x0003)")
	fs.Var(&socImages, "soc-image", "Additional SoC image FILE (repeatable; ids from 0x1000)")
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
	socID := uint32(flshIDSocImagesBase)
	for _, path := range socImages {
		if err := add(socID, path); err != nil {
			return err
		}
		socID++
	}

	if len(images) == 0 {
		return fmt.Errorf("no images: supply --caliptra/--soc-manifest/--mcu-runtime and/or --soc-image")
	}

	img := buildFlashImage(images)
	if err := os.WriteFile(output, img, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}

	fmt.Println("=== AST2700-A2 FLSH image ===")
	off := flshHeaderSize + flshCksumSize + flshInfoSize*len(images)
	for _, im := range images {
		psize := len(flshPad4(im.data))
		fmt.Printf("  image id=%#06x offset=%#010x size=%#010x (%d raw)\n",
			im.id, off, psize, len(im.data))
		off += psize
	}
	fmt.Printf("  image_count=%d header_crc=%#010x total=%d bytes\n",
		len(images), flshCrc32(img[:8]), len(img))
	fmt.Printf("  Output: %s\n", output)
	return nil
}
