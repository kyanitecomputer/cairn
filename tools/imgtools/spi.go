package main

import (
	"crypto/rand"
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	caliptraOffset = 0x00000000
	asthOffset     = 0x00020000
	fmcOffset      = asthOffset + headerSize // 0x20A00

	// A35 boot header: lets the BootMCU locate the A35 entry point without
	// hardcoding an offset that shifts whenever the payload size changes.
	// Written just below the A35 payload (0x800000); read by the BootMCU via
	// memory-mapped SPI (SPI_BASE + a35HeaderOffset). The whole image (incl.
	// this header) is covered by the ASTH/manifest measurement, so the header
	// is not a trust boundary — the magic is only a sanity/staleness check.
	a35HeaderOffset = 0x007F0000
	a35HeaderMagic  = 0xA35EB007
	// a35TextBase is the link/objcopy base of the A35 (tamago) binary: the
	// vaddr of the lowest allocatable section (.text), set by `-T 0x404000000`.
	// The recorded entry offset is e_entry - a35TextBase, i.e. the byte offset
	// of _rt0 within the raw (objcopy -O binary) payload.
	a35TextBase = 0x404000000
)

type rawPayload struct {
	name   string
	path   string
	offset uint64
	data   []byte
}

func cmdSpiImage(args []string) error {
	fs := flag.NewFlagSet("spi-image", flag.ExitOnError)
	var caliptraPath, fmcPath, output, sizeStr string
	var signKeyPath, encryptKeyPath string
	var svn int
	var pspPayload, pspElf, sspPayload, tspPayload string
	var prebuilts repeatFlag

	fs.StringVar(&caliptraPath, "caliptra", "", "Caliptra FW binary (required)")
	fs.StringVar(&fmcPath, "fmc", "", "FMC firmware binary (required)")
	fs.StringVar(&output, "output", "", "Output flash image file")
	fs.StringVar(&output, "o", "", "Output flash image file")
	fs.StringVar(&sizeStr, "size", "", "Force output size (e.g. 16M). Default: auto 64K-aligned")
	fs.IntVar(&svn, "svn", 0, "Secure Version Number")
	fs.Var(&prebuilts, "prebuilt", "Prebuilt TYPE:FILE (repeatable)")
	// PSP = the AST2700 CA35 application processor payload (aligns with SSP/TSP).
	fs.StringVar(&pspPayload, "psp-payload", "", "PSP (CA35) raw payload at offset 0x800000")
	fs.StringVar(&pspElf, "psp-elf", "", "PSP (CA35) ELF (read entry point for the PSP boot header; required with --psp-payload)")
	// Back-compat aliases (deprecated): --a35-payload / --a35-elf.
	fs.StringVar(&pspPayload, "a35-payload", "", "deprecated alias of --psp-payload")
	fs.StringVar(&pspElf, "a35-elf", "", "deprecated alias of --psp-elf")
	fs.StringVar(&sspPayload, "ssp-payload", "", "SSP coprocessor payload at offset 0x1800000")
	fs.StringVar(&tspPayload, "tsp-payload", "", "TSP coprocessor payload at offset 0x1820000")
	fs.StringVar(&signKeyPath, "sign-key", "", "ECDSA P-384 private key PEM for signing")
	fs.StringVar(&encryptKeyPath, "encrypt-key", "", "AES-256 key file (32 bytes) for encryption")
	fs.Parse(args)

	if caliptraPath == "" || fmcPath == "" || output == "" {
		fs.Usage()
		return fmt.Errorf("--caliptra, --fmc, and --output are required")
	}

	caliptraData, err := os.ReadFile(caliptraPath)
	if err != nil {
		return fmt.Errorf("read caliptra: %w", err)
	}
	fmcData, err := os.ReadFile(fmcPath)
	if err != nil {
		return fmt.Errorf("read FMC: %w", err)
	}

	if caliptraOffset+uint64(len(caliptraData)) > asthOffset {
		return fmt.Errorf("Caliptra FW (%d bytes) overflows into ASTH region at 0x%x",
			len(caliptraData), asthOffset)
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

	asthHeader := buildASTHHeader(fmcData, pbEntries, uint32(svn))
	contentEnd := fmcOffset + uint64(len(fmcData))

	type placedPrebuilt struct {
		typeCode uint32
		data     []byte
		offset   uint64
	}
	var placedPBs []placedPrebuilt
	for _, pb := range pbEntries {
		off := alignUp(contentEnd, 16)
		placedPBs = append(placedPBs, placedPrebuilt{pb.typeCode, pb.data, off})
		contentEnd = off + uint64(len(pb.data))
	}

	var payloads []rawPayload
	for _, spec := range []struct {
		name, path string
		offset     uint64
	}{
		{"PSP", pspPayload, 0x800000},
		{"SSP", sspPayload, 0x1800000},
		{"TSP", tspPayload, 0x1820000},
	} {
		if spec.path == "" {
			continue
		}
		data, err := os.ReadFile(spec.path)
		if err != nil {
			return fmt.Errorf("read %s payload: %w", spec.name, err)
		}
		payloads = append(payloads, rawPayload{spec.name, spec.path, spec.offset, data})
		end := spec.offset + uint64(len(data))
		if end > contentEnd {
			contentEnd = end
		}
	}

	for i := 0; i < len(payloads)-1; i++ {
		for j := i + 1; j < len(payloads); j++ {
			a, b := payloads[i], payloads[j]
			aEnd := a.offset + uint64(len(a.data))
			if a.offset < b.offset+uint64(len(b.data)) && b.offset < aEnd {
				return fmt.Errorf("%s (ends 0x%x) overlaps %s (starts 0x%x)",
					a.name, aEnd, b.name, b.offset)
			}
		}
	}

	// Build the PSP (CA35) boot header from the ELF entry point.
	var pspHeader []byte
	var pspEntryOff uint32
	if pspPayload != "" {
		if pspElf == "" {
			return fmt.Errorf("--psp-elf is required when --psp-payload is set (needed to record the entry offset)")
		}
		var pspLen uint32
		for _, p := range payloads {
			if p.name == "PSP" {
				pspLen = uint32(len(p.data))
			}
		}
		ef, err := elf.Open(pspElf)
		if err != nil {
			return fmt.Errorf("open --psp-elf: %w", err)
		}
		entry := ef.Entry
		ef.Close()
		if entry < a35TextBase {
			return fmt.Errorf("PSP ELF entry 0x%x is below text base 0x%x", entry, uint64(a35TextBase))
		}
		pspEntryOff = uint32(entry - a35TextBase)
		pspHeader = make([]byte, 16)
		binary.LittleEndian.PutUint32(pspHeader[0:], a35HeaderMagic)
		binary.LittleEndian.PutUint32(pspHeader[4:], pspEntryOff)
		binary.LittleEndian.PutUint32(pspHeader[8:], pspLen)
		binary.LittleEndian.PutUint32(pspHeader[12:], a35HeaderMagic^pspEntryOff^pspLen)
		if end := uint64(a35HeaderOffset) + uint64(len(pspHeader)); end > contentEnd {
			contentEnd = end
		}
	}

	var imageSize uint64
	if sizeStr != "" {
		imageSize, err = parseSize(sizeStr)
		if err != nil {
			return fmt.Errorf("parse --size: %w", err)
		}
		if imageSize < contentEnd {
			return fmt.Errorf("requested size 0x%x < content end 0x%x", imageSize, contentEnd)
		}
	} else {
		imageSize = alignUp(contentEnd, 0x10000)
	}

	image := make([]byte, imageSize)
	for i := range image {
		image[i] = 0xFF
	}

	copy(image[caliptraOffset:], caliptraData)
	copy(image[asthOffset:], asthHeader)
	copy(image[fmcOffset:], fmcData)

	for _, pb := range placedPBs {
		copy(image[pb.offset:], pb.data)
	}
	for _, p := range payloads {
		copy(image[p.offset:], p.data)
	}
	if pspHeader != nil {
		copy(image[a35HeaderOffset:], pspHeader)
	}

	if encryptKeyPath != "" {
		aesKey, err := loadAESKey(encryptKeyPath)
		if err != nil {
			return fmt.Errorf("load AES key: %w", err)
		}
		iv := make([]byte, 16)
		if _, err := rand.Read(iv); err != nil {
			return fmt.Errorf("generate IV: %w", err)
		}
		fmcEnd := fmcOffset + uint64(len(fmcData))
		encrypted, err := aesEncryptCTR(aesKey, iv, image[fmcOffset:fmcEnd])
		if err != nil {
			return fmt.Errorf("AES encrypt: %w", err)
		}
		copy(image[fmcOffset:], encrypted)
		copy(image[asthOffset+uint64(aesIVOffset):], iv[:16])
		fmt.Println("AES-256-CTR encryption applied to FMC region")
	}

	if signKeyPath != "" {
		ecKey, err := loadECDSAPrivateKey(signKeyPath)
		if err != nil {
			return fmt.Errorf("load sign key: %w", err)
		}
		asthEnd := asthOffset + headerSize + uint64(len(fmcData))
		for _, pb := range placedPBs {
			end := pb.offset + uint64(len(pb.data))
			if end > asthEnd {
				asthEnd = end
			}
		}
		digest := sha384Sum(image[asthOffset:asthEnd])
		sig, err := ecdsaSignDigest(ecKey, digest)
		if err != nil {
			return fmt.Errorf("ECDSA sign: %w", err)
		}
		copy(image[asthOffset+eccSigOffset:], sig[:eccSigLen])
		fmt.Println("ECDSA P-384 signature applied to ASTH region")
	}

	if err := os.WriteFile(output, image, 0644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}

	fmt.Printf("=== AST2700 SPI Flash Image ===\n")
	fmt.Printf("  Caliptra FW:  %s (%d bytes @ 0x%08x)\n", caliptraPath, len(caliptraData), caliptraOffset)
	fmt.Printf("  ASTH header:  %d bytes @ 0x%08x (version=%d, svn=%d, %d prebuilt(s))\n",
		headerSize, asthOffset, asthVersion, svn, len(pbEntries))
	fmt.Printf("  FMC binary:   %s (%d bytes @ 0x%08x)\n", fmcPath, len(fmcData), fmcOffset)
	prebuiltNames := map[uint32]string{
		1: "DDR4-IMEM", 2: "DDR4-DMEM", 3: "DDR4-2D-IMEM", 4: "DDR4-2D-DMEM",
		5: "DDR5-IMEM", 6: "DDR5-DMEM", 7: "DP-FW", 8: "UEFI",
	}
	for _, pb := range placedPBs {
		name := prebuiltNames[pb.typeCode]
		if name == "" {
			name = fmt.Sprintf("type-0x%x", pb.typeCode)
		}
		fmt.Printf("  Prebuilt[0x%x]: %s (%d bytes @ 0x%08x)\n",
			pb.typeCode, name, len(pb.data), pb.offset)
	}
	for _, p := range payloads {
		fmt.Printf("  %s payload:  %s (%d bytes @ 0x%08x)\n", p.name, p.path, len(p.data), p.offset)
	}
	if pspHeader != nil {
		fmt.Printf("  PSP header:   magic=0x%08x entry_off=0x%x @ 0x%08x (from %s)\n",
			uint32(a35HeaderMagic), pspEntryOff, uint64(a35HeaderOffset), pspElf)
	}
	fmt.Printf("  Content end:  0x%08x\n", contentEnd)
	fmt.Printf("  Image size:   %d bytes (%d KB)\n", imageSize, imageSize/1024)
	fmt.Printf("  Output:       %s\n", output)
	return nil
}
