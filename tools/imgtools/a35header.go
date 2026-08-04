package main

import (
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
)

// cmdA35Header prepends the 16-byte A35 boot header
// {magic, entry_off, payload_len, check} to a raw CA35 payload, so the BootMCU
// reads the entry offset from the image instead of a hardcoded constant that
// must be bumped whenever the payload size changes.
//
// entry_off is the byte offset of the ELF entry (_rt0) within the raw payload,
// i.e. e_entry - firstLoadableVMA — the same value validate-layout computes and
// the historical A2_CA35_ENTRY_OFF baked in. The 16-byte layout and magic match
// the A1 PSP header (spi.go) and embassy_aspeed::manifest::RAW_A35_HEADER_* on
// the BootMCU read side.
func cmdA35Header(args []string) error {
	fs := flag.NewFlagSet("a35-header", flag.ExitOnError)
	elfPath := fs.String("elf", "", "CA35 payload ELF (source of the entry offset)")
	inPath := fs.String("in", "", "raw payload binary (objcopy -O binary output)")
	outPath := fs.String("out", "", "output file: header || payload")
	fs.Parse(args)
	if *elfPath == "" || *inPath == "" || *outPath == "" {
		return fmt.Errorf("usage: imgtools a35-header --elf <elf> --in <raw.bin> --out <out.bin>")
	}

	f, err := elf.Open(*elfPath)
	if err != nil {
		return fmt.Errorf("open --elf: %w", err)
	}
	defer f.Close()
	base := firstLoadableVMA(f)
	if f.Entry < base {
		return fmt.Errorf("ELF entry 0x%x below first loadable VMA 0x%x", f.Entry, base)
	}
	entryOff := uint32(f.Entry - base)

	payload, err := os.ReadFile(*inPath)
	if err != nil {
		return fmt.Errorf("read --in: %w", err)
	}
	payloadLen := uint32(len(payload))

	hdr := make([]byte, 16)
	binary.LittleEndian.PutUint32(hdr[0:], a35HeaderMagic)
	binary.LittleEndian.PutUint32(hdr[4:], entryOff)
	binary.LittleEndian.PutUint32(hdr[8:], payloadLen)
	binary.LittleEndian.PutUint32(hdr[12:], a35HeaderMagic^entryOff^payloadLen)

	out := make([]byte, 0, len(hdr)+len(payload))
	out = append(out, hdr...)
	out = append(out, payload...)
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		return fmt.Errorf("write --out: %w", err)
	}
	fmt.Printf("a35-header: entry_off=0x%X payload_len=0x%X -> %s (%d bytes)\n",
		entryOff, payloadLen, *outPath, len(out))
	return nil
}

// firstLoadableVMA returns the VMA that raw byte 0 of `objcopy -O binary`
// corresponds to: the Go pvh note address when present, else the lowest
// allocatable non-debug section address. Mirrors validate-layout's binVMAStart.
func firstLoadableVMA(f *elf.File) uint64 {
	if pvh := f.Section(".note.go.pvh"); pvh != nil && pvh.Addr > 0 {
		return pvh.Addr
	}
	start := ^uint64(0)
	for _, sec := range f.Sections {
		if sec.Addr == 0 || sec.Size == 0 {
			continue
		}
		if strings.HasPrefix(sec.Name, ".debug") ||
			sec.Name == ".symtab" || sec.Name == ".strtab" || sec.Name == ".shstrtab" {
			continue
		}
		if sec.Addr < start {
			start = sec.Addr
		}
	}
	return start
}
