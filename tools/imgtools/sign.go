package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

func loadECDSAPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ecKey, ok := key.(*ecdsa.PrivateKey); ok {
			return ecKey, nil
		}
		return nil, fmt.Errorf("PKCS8 key in %s is not ECDSA", path)
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func loadRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("PKCS8 key in %s is not RSA", path)
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func loadAESKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("AES key must be exactly 32 bytes, got %d", len(data))
	}
	return data, nil
}

// ecdsaSignDigest signs a digest with ECDSA and returns raw r||s bytes.
func ecdsaSignDigest(key *ecdsa.PrivateKey, digest []byte) ([]byte, error) {
	r, s, err := ecdsa.Sign(rand.Reader, key, digest)
	if err != nil {
		return nil, err
	}
	curveBytes := (key.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*curveBytes)
	r.FillBytes(sig[:curveBytes])
	s.FillBytes(sig[curveBytes:])
	return sig, nil
}

// rsaSignRaw signs data using raw RSA PKCS1v1.5 (socsec-compatible).
// When littleEndian is true, input and output bytes are reversed to match
// the ASPEED ROM's little-endian key ordering convention.
func rsaSignRaw(key *rsa.PrivateKey, digest []byte, littleEndian bool) ([]byte, error) {
	input := make([]byte, len(digest))
	copy(input, digest)
	if littleEndian {
		reverseBytes(input)
	}
	sig, err := rsa.SignPKCS1v15(nil, key, 0, input)
	if err != nil {
		return nil, err
	}
	if littleEndian {
		reverseBytes(sig)
	}
	return sig, nil
}

func aesEncryptCTR(key, iv, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	ciphertext := make([]byte, len(plaintext))
	stream := cipher.NewCTR(block, iv)
	stream.XORKeyStream(ciphertext, plaintext)
	return ciphertext, nil
}

func aesEncryptGCM(key, nonce, plaintext, aad []byte) (ciphertext, tag []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	sealed := aesGCM.Seal(nil, nonce, plaintext, aad)
	tagSize := aesGCM.Overhead()
	ciphertext = sealed[:len(sealed)-tagSize]
	tag = sealed[len(sealed)-tagSize:]
	return ciphertext, tag, nil
}

// reverseGCMTag applies the 4-byte group reversal that the ASPEED ROM expects.
// Input:  [A B C D | E F G H | I J K L | M N O P]
// Output: [M N O P | I J K L | E F G H | A B C D]
func reverseGCMTag(tag []byte) []byte {
	rev := make([]byte, 16)
	for i := 0; i < 16; i += 4 {
		copy(rev[i:i+4], tag[12-i:16-i])
	}
	return rev
}
