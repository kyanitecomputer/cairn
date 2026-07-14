package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func cmdFmcImage(args []string) error {
	fs := flag.NewFlagSet("fmc-image", flag.ExitOnError)
	var output, signKeyPath, encryptKeyPath string
	var uartHeader bool
	var svn int
	var eccKeyIdx int
	var prebuilts repeatFlag
	var rawPayloads repeatFlag

	fs.StringVar(&output, "output", "", "Output image file")
	fs.StringVar(&output, "o", "", "Output image file")
	fs.BoolVar(&uartHeader, "uart-header", false, "Add 4-byte LE UART size prefix")
	fs.IntVar(&svn, "svn", 0, "Secure Version Number")
	fs.IntVar(&eccKeyIdx, "ecc-key-idx", 0, "ECC key index hint")
	fs.Var(&prebuilts, "prebuilt", "Prebuilt entry TYPE:FILE (repeatable)")
	fs.Var(&rawPayloads, "raw-payload", "Raw payload OFFSET:FILE (repeatable)")
	fs.StringVar(&signKeyPath, "sign-key", "", "ECDSA P-384 private key PEM for signing")
	fs.StringVar(&encryptKeyPath, "encrypt-key", "", "AES-256 key file (32 bytes) for encryption")
	fs.Parse(args)

	if fs.NArg() < 1 || output == "" {
		fmt.Fprintln(os.Stderr, "Usage: imgtools fmc-image [flags] <fmc.bin>")
		fs.PrintDefaults()
		return fmt.Errorf("FMC input and --output are required")
	}
	fmcPath := fs.Arg(0)

	fmcData, err := os.ReadFile(fmcPath)
	if err != nil {
		return fmt.Errorf("read FMC: %w", err)
	}

	var pbEntries []prebuiltEntry
	for _, spec := range prebuilts {
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("--prebuilt must be TYPE:FILE, got: %s", spec)
		}
		tc, err := strconv.ParseUint(parts[0], 0, 32)
		if err != nil {
			return fmt.Errorf("parse prebuilt type %q: %w", parts[0], err)
		}
		data, err := os.ReadFile(parts[1])
		if err != nil {
			return fmt.Errorf("read prebuilt %s: %w", parts[1], err)
		}
		pbEntries = append(pbEntries, prebuiltEntry{typeCode: uint32(tc), data: data})
	}

	header := buildASTHHeader(fmcData, pbEntries, uint32(svn))
	binary.LittleEndian.PutUint32(header[8:12], uint32(eccKeyIdx))

	payload := make([]byte, 0, headerSize+len(fmcData))
	payload = append(payload, header...)
	payload = append(payload, fmcData...)

	for _, pb := range pbEntries {
		pad := int(alignUp(uint64(len(payload)), 16)) - len(payload)
		payload = append(payload, make([]byte, pad)...)
		payload = append(payload, pb.data...)
	}

	const asthFlashOffset = 0x00020000
	for _, spec := range rawPayloads {
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("--raw-payload must be OFFSET:FILE, got: %s", spec)
		}
		flashOffset, err := strconv.ParseUint(parts[0], 0, 64)
		if err != nil {
			return fmt.Errorf("parse raw-payload offset %q: %w", parts[0], err)
		}
		if flashOffset < asthFlashOffset {
			return fmt.Errorf("raw payload offset 0x%x is before ASTH offset 0x%x", flashOffset, asthFlashOffset)
		}
		fileOffset := int(flashOffset - asthFlashOffset)
		data, err := os.ReadFile(parts[1])
		if err != nil {
			return fmt.Errorf("read raw payload %s: %w", parts[1], err)
		}
		if len(payload) < fileOffset {
			payload = append(payload, make([]byte, fileOffset-len(payload))...)
		}
		if len(payload) < fileOffset+len(data) {
			payload = append(payload, make([]byte, fileOffset+len(data)-len(payload))...)
		}
		copy(payload[fileOffset:], data)
	}

	padN := int(alignUp(uint64(len(payload)), 4)) - len(payload)
	payload = append(payload, make([]byte, padN)...)

	if encryptKeyPath != "" {
		aesKey, err := loadAESKey(encryptKeyPath)
		if err != nil {
			return fmt.Errorf("load AES key: %w", err)
		}
		iv := make([]byte, 16)
		if _, err := rand.Read(iv); err != nil {
			return fmt.Errorf("generate IV: %w", err)
		}
		encrypted, err := aesEncryptCTR(aesKey, iv, payload[headerSize:])
		if err != nil {
			return fmt.Errorf("AES encrypt: %w", err)
		}
		copy(payload[headerSize:], encrypted)
		copy(payload[aesIVOffset:aesIVOffset+16], iv)
		fmt.Println("AES-256-CTR encryption applied")
	}

	if signKeyPath != "" {
		ecKey, err := loadECDSAPrivateKey(signKeyPath)
		if err != nil {
			return fmt.Errorf("load sign key: %w", err)
		}
		digest := sha384Sum(payload)
		sig, err := ecdsaSignDigest(ecKey, digest)
		if err != nil {
			return fmt.Errorf("ECDSA sign: %w", err)
		}
		copy(payload[eccSigOffset:eccSigOffset+eccSigLen], sig)
		fmt.Println("ECDSA P-384 signature applied")
	}

	var outData []byte
	if uartHeader {
		outData = make([]byte, 4+len(payload))
		binary.LittleEndian.PutUint32(outData[0:4], uint32(len(payload)))
		copy(outData[4:], payload)
	} else {
		outData = payload
	}

	if err := os.WriteFile(output, outData, 0644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}

	fmt.Printf("FMC binary:   %s (%d bytes)\n", fmcPath, len(fmcData))
	fmt.Printf("SHA384:       %x\n", sha384Sum(fmcData)[:16])
	fmt.Printf("ASTH header:  %d bytes (magic=ASTH, version=%d, svn=%d, %d prebuilt(s))\n",
		headerSize, asthVersion, svn, len(pbEntries))
	if uartHeader {
		fmt.Printf("UART image:   %s (%d bytes, 4-byte LE size prefix)\n", output, len(outData))
	} else {
		fmt.Printf("Stage-2 image: %s (%d bytes)\n", output, len(outData))
	}
	return nil
}
