package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"flag"
	"fmt"
	"hash"
	"os"
)

// ROT header format for AST2600/AST10x0 secure boot (socsec-compatible).
//
// 32 bytes (8 x uint32 LE) placed at a SoC-specific offset:
//
//	aes_data_offset, enc_offset, sign_image_size, signature_offset,
//	revision_low, revision_high, reserved, checksum
//
// The checksum ensures all 8 fields sum to zero (mod 2^32).
const rotHeaderSize = 32

type socConfig struct {
	headerOffset uint32
	encOffset    uint32
	maxImageSize int
}

var socConfigs = map[string]socConfig{
	"2600": {headerOffset: 0x20, encOffset: 0x50, maxImageSize: 60 * 1024},
	"1030": {headerOffset: 0x400, encOffset: 0x430, maxImageSize: 768 * 1024},
	"1060": {headerOffset: 0x400, encOffset: 0x430, maxImageSize: 768 * 1024},
}

func cmdSecureImage(args []string) error {
	fs := flag.NewFlagSet("secure-image", flag.ExitOnError)
	var (
		soc          string
		algorithm    string
		signKeyPath  string
		encKeyPath   string
		output       string
		revisionID   int
	)

	fs.StringVar(&soc, "soc", "", "SoC version: 2600, 1030, 1060")
	fs.StringVar(&algorithm, "algorithm", "", "Algorithm: ECDSA384, RSA2048_SHA256, RSA4096_SHA512, AES_GCM")
	fs.StringVar(&signKeyPath, "sign-key", "", "Signing key (PEM): ECDSA or RSA private key")
	fs.StringVar(&encKeyPath, "encrypt-key", "", "AES-256 key file (32 bytes) for AES_GCM")
	fs.StringVar(&output, "output", "", "Output signed image")
	fs.StringVar(&output, "o", "", "Output signed image")
	fs.IntVar(&revisionID, "revision", 0, "Anti-rollback revision ID (0-64)")
	fs.Parse(args)

	if fs.NArg() < 1 || soc == "" || algorithm == "" || output == "" {
		fmt.Fprintln(os.Stderr, "Usage: imgtools secure-image [flags] <input.bin>")
		fs.PrintDefaults()
		return fmt.Errorf("--soc, --algorithm, --output, and input file are required")
	}
	inputPath := fs.Arg(0)

	cfg, ok := socConfigs[soc]
	if !ok {
		return fmt.Errorf("unsupported SoC: %s (supported: 2600, 1030, 1060)", soc)
	}

	if revisionID < 0 || revisionID > 64 {
		return fmt.Errorf("revision ID must be 0-64, got %d", revisionID)
	}

	imageData, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	if len(imageData) > cfg.maxImageSize {
		return fmt.Errorf("image (%d bytes) exceeds max %d bytes for %s",
			len(imageData), cfg.maxImageSize, soc)
	}

	signImageSize := uint32(alignUp(uint64(len(imageData)), 512))

	var revLow, revHigh uint32
	for i := 0; i < revisionID; i++ {
		if i < 32 {
			revLow |= 1 << i
		} else {
			revHigh |= 1 << (i - 32)
		}
	}

	switch algorithm {
	case "ECDSA384":
		return secureECDSA(cfg, imageData, signImageSize, signKeyPath,
			revLow, revHigh, output)
	case "RSA2048_SHA256":
		return secureRSA(cfg, imageData, signImageSize, signKeyPath,
			256, crypto.SHA256, sha256.New, revLow, revHigh, output)
	case "RSA4096_SHA512":
		return secureRSA(cfg, imageData, signImageSize, signKeyPath,
			512, crypto.SHA512, sha512.New, revLow, revHigh, output)
	case "AES_GCM":
		return secureAESGCM(cfg, imageData, signImageSize, encKeyPath,
			revLow, revHigh, output)
	default:
		return fmt.Errorf("unsupported algorithm: %s", algorithm)
	}
}

func buildROTHeader(aesDataOff, encOff, signImgSize, sigOff, revLow, revHigh, reserved uint32) []byte {
	checksum := -(aesDataOff + encOff + signImgSize + sigOff + revLow + revHigh + reserved)
	hdr := make([]byte, rotHeaderSize)
	binary.LittleEndian.PutUint32(hdr[0:], aesDataOff)
	binary.LittleEndian.PutUint32(hdr[4:], encOff)
	binary.LittleEndian.PutUint32(hdr[8:], signImgSize)
	binary.LittleEndian.PutUint32(hdr[12:], sigOff)
	binary.LittleEndian.PutUint32(hdr[16:], revLow)
	binary.LittleEndian.PutUint32(hdr[20:], revHigh)
	binary.LittleEndian.PutUint32(hdr[24:], reserved)
	binary.LittleEndian.PutUint32(hdr[28:], checksum)
	return hdr
}

func secureECDSA(cfg socConfig, imageData []byte, signImageSize uint32,
	signKeyPath string, revLow, revHigh uint32, output string) error {

	if signKeyPath == "" {
		return fmt.Errorf("--sign-key is required for ECDSA384")
	}
	ecKey, err := loadECDSAPrivateKey(signKeyPath)
	if err != nil {
		return fmt.Errorf("load ECDSA key: %w", err)
	}

	sigOffset := signImageSize
	hdr := buildROTHeader(0, 0, signImageSize, sigOffset, revLow, revHigh, 0)

	out := make([]byte, signImageSize+eccSigLen)
	copy(out, imageData)
	copy(out[cfg.headerOffset:], hdr)

	digest := sha384Sum(out[:signImageSize])
	sig, err := ecdsaSignDigest(ecKey, digest)
	if err != nil {
		return fmt.Errorf("ECDSA sign: %w", err)
	}
	copy(out[sigOffset:], sig[:eccSigLen])

	if err := os.WriteFile(output, out, 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	fmt.Printf("Secure image (ECDSA384): %s (%d bytes, signed region: %d bytes)\n",
		output, len(out), signImageSize)
	return nil
}

func secureRSA(cfg socConfig, imageData []byte, signImageSize uint32,
	signKeyPath string, rsaBytes int, hashType crypto.Hash, newHash func() hash.Hash,
	revLow, revHigh uint32, output string) error {

	if signKeyPath == "" {
		return fmt.Errorf("--sign-key is required for RSA signing")
	}
	rsaKey, err := loadRSAPrivateKey(signKeyPath)
	if err != nil {
		return fmt.Errorf("load RSA key: %w", err)
	}

	sigOffset := signImageSize
	hdr := buildROTHeader(0, 0, signImageSize, sigOffset, revLow, revHigh, 0)

	out := make([]byte, uint64(signImageSize)+uint64(rsaBytes))
	copy(out, imageData)
	copy(out[cfg.headerOffset:], hdr)

	h := newHash()
	h.Write(out[:signImageSize])
	digest := h.Sum(nil)

	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, hashType, digest)
	if err != nil {
		return fmt.Errorf("RSA sign: %w", err)
	}
	reverseBytes(sig)
	copy(out[sigOffset:], sig)

	if err := os.WriteFile(output, out, 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	fmt.Printf("Secure image (RSA%d): %s (%d bytes, signed region: %d bytes)\n",
		rsaKey.N.BitLen(), output, len(out), signImageSize)
	return nil
}

func secureAESGCM(cfg socConfig, imageData []byte, signImageSize uint32,
	encKeyPath string, revLow, revHigh uint32, output string) error {

	if encKeyPath == "" {
		return fmt.Errorf("--encrypt-key is required for AES_GCM")
	}
	aesKey, err := loadAESKey(encKeyPath)
	if err != nil {
		return fmt.Errorf("load AES key: %w", err)
	}

	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	iv := make([]byte, 16)
	copy(iv, nonce)
	iv[15] = 0x01

	aesDataOff := signImageSize
	sigOffset := signImageSize + 16
	encOffset := cfg.encOffset

	hdr := buildROTHeader(aesDataOff, encOffset, signImageSize, sigOffset, revLow, revHigh, 0)

	out := make([]byte, signImageSize+32) // +16 IV +16 tag
	copy(out, imageData)
	copy(out[cfg.headerOffset:], hdr)

	aad := make([]byte, encOffset)
	copy(aad, out[:encOffset])

	ciphertext, tag, err := aesEncryptGCM(aesKey, nonce, out[encOffset:signImageSize], aad)
	if err != nil {
		return fmt.Errorf("AES-GCM encrypt: %w", err)
	}
	copy(out[encOffset:], ciphertext)
	copy(out[aesDataOff:], iv)
	copy(out[sigOffset:], reverseGCMTag(tag))

	if err := os.WriteFile(output, out, 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	fmt.Printf("Secure image (AES_GCM): %s (%d bytes, encrypted from offset 0x%x)\n",
		output, len(out), encOffset)
	return nil
}
