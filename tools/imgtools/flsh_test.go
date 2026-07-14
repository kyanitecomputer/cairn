package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestBuildFlashImageRoundtrip mirrors caliptra-sw hw-model/src/flash_image.rs
// test_build_flash_image_roundtrip to guarantee our port is byte-compatible with
// the format the MCU ROM parses.
func TestBuildFlashImageRoundtrip(t *testing.T) {
	fw := bytes.Repeat([]byte{0xAA}, 100)
	manifest := bytes.Repeat([]byte{0xBB}, 200)
	mcu := bytes.Repeat([]byte{0xCC}, 50)

	img := buildFlashImage([]flshImage{
		{flshIDCaliptraFmcRt, fw},
		{flshIDSocManifest, manifest},
		{flshIDMcuRt, mcu},
	})
	if len(img) == 0 {
		t.Fatal("empty image")
	}

	// Flash header.
	if string(img[0:4]) != "FLSH" {
		t.Fatalf("magic = %q, want FLSH", img[0:4])
	}
	if v := binary.LittleEndian.Uint16(img[4:6]); v != flshHeaderVersion {
		t.Fatalf("version = %d, want %d", v, flshHeaderVersion)
	}
	if c := binary.LittleEndian.Uint16(img[6:8]); c != 3 {
		t.Fatalf("image_count = %d, want 3", c)
	}
	hdrOff := binary.LittleEndian.Uint32(img[8:12])
	if hdrOff != flshHeaderSize {
		t.Fatalf("image_headers_offset = %d, want %d", hdrOff, flshHeaderSize)
	}
	if got, want := binary.LittleEndian.Uint32(img[12:16]), flshChecksum(img[:12]); got != want {
		t.Fatalf("header_checksum = %#x, want %#x", got, want)
	}

	wantIDs := []uint32{flshIDCaliptraFmcRt, flshIDSocManifest, flshIDMcuRt}
	for i := 0; i < 3; i++ {
		off := int(hdrOff) + i*flshImageHdrSize
		h := img[off : off+flshImageHdrSize]
		id := binary.LittleEndian.Uint32(h[0:4])
		ioff := binary.LittleEndian.Uint32(h[4:8])
		size := binary.LittleEndian.Uint32(h[8:12])
		ick := binary.LittleEndian.Uint32(h[12:16])
		hck := binary.LittleEndian.Uint32(h[16:20])

		if id != wantIDs[i] {
			t.Fatalf("image[%d] id = %#x, want %#x", i, id, wantIDs[i])
		}
		if size != 256 { // 100/200/50 all pad to 256
			t.Fatalf("image[%d] size = %d, want 256", i, size)
		}
		if want := flshChecksum(h[:16]); hck != want {
			t.Fatalf("image[%d] header_checksum = %#x, want %#x", i, hck, want)
		}
		data := img[ioff : ioff+size]
		if want := flshChecksum(data); ick != want {
			t.Fatalf("image[%d] image_checksum = %#x, want %#x", i, ick, want)
		}
	}
}

func TestBuildFlashImageEmpty(t *testing.T) {
	if img := buildFlashImage(nil); img != nil {
		t.Fatalf("expected nil for no images, got %d bytes", len(img))
	}
}
