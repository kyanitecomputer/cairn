package main

import (
	"crypto/sha512"
	"encoding/binary"
)

const (
	asthMagic    = 0x48545341 // 'ASTH'
	asthVersion  = 2
	preambleSize = 1792
	bodySize     = 768
	headerSize   = preambleSize + bodySize // 2560
	eccSigLen    = 96
	lmsSigLen    = 1620
	sha384Len    = 48

	prebuiltEntrySize = 56 // type(4) + size(4) + sha384(48)
	maxPrebuilts      = 12

	eccSigOffset = 16 // offset of ecc_sig within preamble
	aesIVOffset  = preambleSize - 60 // first 16 bytes of the 60-byte raz field
)

type prebuiltEntry struct {
	typeCode uint32
	data     []byte
}

func sha384Sum(data []byte) []byte {
	h := sha512.Sum384(data)
	return h[:]
}

// buildASTHHeader creates a 2560-byte ASTH secure boot header.
//
// Preamble (1792 bytes):
//
//	magic(4) + version(4) + ecc_key_idx(4) + lms_key_idx(4) +
//	ecc_sig(96) + lms_sig(1620) + raz(60)
//
// Body (768 bytes):
//
//	svn(4) + fmc_size(4) + sha384(48) + prebuilt_entries(712)
func buildASTHHeader(fmcData []byte, prebuilts []prebuiltEntry, svn uint32) []byte {
	header := make([]byte, headerSize)

	binary.LittleEndian.PutUint32(header[0:4], asthMagic)
	binary.LittleEndian.PutUint32(header[4:8], asthVersion)
	// ecc_key_idx(4), lms_key_idx(4) = 0
	// ecc_sig(96), lms_sig(1620), raz(60) = zeros (unsigned)

	body := header[preambleSize:]
	binary.LittleEndian.PutUint32(body[0:4], svn)
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(fmcData)))
	copy(body[8:8+sha384Len], sha384Sum(fmcData))

	entryStart := 8 + sha384Len // = 56
	for i, pb := range prebuilts {
		if i >= maxPrebuilts {
			break
		}
		off := entryStart + i*prebuiltEntrySize
		binary.LittleEndian.PutUint32(body[off:off+4], pb.typeCode)
		binary.LittleEndian.PutUint32(body[off+4:off+8], uint32(len(pb.data)))
		copy(body[off+8:off+8+sha384Len], sha384Sum(pb.data))
	}

	return header
}
