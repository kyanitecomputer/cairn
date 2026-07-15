package main

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// TestBuildFlashImageBuild mirrors caliptra-mcu-sw builder/src/flash_image.rs
// test_flash_image_build (at the AST2700 A1/A2 pinned revision) to guarantee our
// port is byte-compatible with the format the MCU ROM parses.
func TestBuildFlashImageBuild(t *testing.T) {
	caliptra := []byte("Caliptra Firmware Data - ABCDEFGH")
	manifest := []byte("Soc Manifest Data - 123456789")
	mcu := []byte("MCU Runtime Data - QWERTYUI")
	soc1 := []byte("Soc Image 1 Data - ZXCVBNMLKJ")
	soc2 := []byte("Soc Image 2 Data - POIUYTREWQ")

	data := buildFlashImage([]flshImage{
		{flshIDCaliptraFmcRt, caliptra},
		{flshIDSocManifest, manifest},
		{flshIDMcuRt, mcu},
		{flshIDSocImagesBase, soc1},
		{flshIDSocImagesBase + 1, soc2},
	})
	if len(data) == 0 {
		t.Fatal("empty image")
	}

	// Header: magic big-endian "FLSH", version + count little-endian.
	if magic := binary.BigEndian.Uint32(data[0:4]); magic != 0x464C5348 {
		t.Fatalf("magic = %#x, want 0x464C5348", magic)
	}
	if v := binary.LittleEndian.Uint16(data[4:6]); v != flshHeaderVersion {
		t.Fatalf("version = %d, want %d", v, flshHeaderVersion)
	}
	if c := binary.LittleEndian.Uint16(data[6:8]); c != 5 {
		t.Fatalf("image_count = %d, want 5", c)
	}

	// Checksums: header CRC over [0..8], payload CRC over [16..].
	if got, want := binary.LittleEndian.Uint32(data[8:12]), crc32.ChecksumIEEE(data[0:8]); got != want {
		t.Fatalf("header_crc = %#x, want %#x", got, want)
	}
	if got, want := binary.LittleEndian.Uint32(data[12:16]), crc32.ChecksumIEEE(data[16:]); got != want {
		t.Fatalf("payload_crc = %#x, want %#x", got, want)
	}

	type want struct {
		id   uint32
		body []byte
	}
	wants := []want{
		{flshIDCaliptraFmcRt, caliptra},
		{flshIDSocManifest, manifest},
		{flshIDMcuRt, mcu},
		{flshIDSocImagesBase, soc1},
		{flshIDSocImagesBase + 1, soc2},
	}
	for i, w := range wants {
		off := flshHeaderSize + flshCksumSize + flshInfoSize*i
		id := binary.LittleEndian.Uint32(data[off : off+4])
		ioff := binary.LittleEndian.Uint32(data[off+4 : off+8])
		size := binary.LittleEndian.Uint32(data[off+8 : off+12])

		if id != w.id {
			t.Fatalf("image[%d] id = %#x, want %#x", i, id, w.id)
		}
		wantSize := (len(w.body) + 3) &^ 3
		if int(size) != wantSize {
			t.Fatalf("image[%d] size = %d, want %d (4-byte padded)", i, size, wantSize)
		}
		got := data[ioff : ioff+uint32(len(w.body))]
		if string(got) != string(w.body) {
			t.Fatalf("image[%d] data mismatch", i)
		}
	}
}

func TestBuildFlashImageEmpty(t *testing.T) {
	if img := buildFlashImage(nil); img != nil {
		t.Fatalf("expected nil for no images, got %d bytes", len(img))
	}
}
