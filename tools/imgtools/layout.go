package main

import (
	"debug/elf"
	"flag"
	"fmt"
	"strings"
)

func cmdValidateLayout(args []string) error {
	fs := flag.NewFlagSet("validate-layout", flag.ExitOnError)
	var a35LoadAddr, a35EntryOffset uint64
	var sspLoad, sspSize, tspLoad, tspSize uint64
	var ramStart, ramSize, ramStackOffset uint64
	var hasEntryOffset bool

	fs.Func("a35-load-addr", "A35 load address (default 0x83FFFF60)", func(s string) error {
		v, err := parseHexAddr(s)
		a35LoadAddr = v
		return err
	})
	fs.Func("a35-entry-offset", "A35 entry raw offset", func(s string) error {
		v, err := parseHexAddr(s)
		a35EntryOffset = v
		hasEntryOffset = true
		return err
	})
	fs.Func("ssp-load", "SSP load address (default 0xAC000000)", func(s string) error {
		v, err := parseHexAddr(s)
		sspLoad = v
		return err
	})
	fs.Func("ssp-size", "SSP size (default 0x20000)", func(s string) error {
		v, err := parseHexAddr(s)
		sspSize = v
		return err
	})
	fs.Func("tsp-load", "TSP load address (default 0xAE000000)", func(s string) error {
		v, err := parseHexAddr(s)
		tspLoad = v
		return err
	})
	fs.Func("tsp-size", "TSP size (default 0x20000)", func(s string) error {
		v, err := parseHexAddr(s)
		tspSize = v
		return err
	})
	fs.Func("ram-start", "RAM start (default 0x400000000)", func(s string) error {
		v, err := parseHexAddr(s)
		ramStart = v
		return err
	})
	fs.Func("ram-size", "RAM size (default 0x40000000)", func(s string) error {
		v, err := parseHexAddr(s)
		ramSize = v
		return err
	})
	fs.Func("ram-stack-offset", "Stack offset from RAM end (default 0x100000)", func(s string) error {
		v, err := parseHexAddr(s)
		ramStackOffset = v
		return err
	})

	if a35LoadAddr == 0 {
		a35LoadAddr = 0x83FFFF60
	}
	if sspLoad == 0 {
		sspLoad = 0xAC000000
	}
	if sspSize == 0 {
		sspSize = 0x20000
	}
	if tspLoad == 0 {
		tspLoad = 0xAE000000
	}
	if tspSize == 0 {
		tspSize = 0x20000
	}
	if ramStart == 0 {
		ramStart = 0x400000000
	}
	if ramSize == 0 {
		ramSize = 0x40000000
	}
	if ramStackOffset == 0 {
		ramStackOffset = 0x100000
	}

	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: imgtools validate-layout [flags] <tamago.elf>")
	}
	elfPath := fs.Arg(0)

	f, err := elf.Open(elfPath)
	if err != nil {
		return fmt.Errorf("open ELF: %w", err)
	}
	defer f.Close()

	var binVMAStart, bssEnd, dataEnd uint64
	binVMAStart = ^uint64(0)

	pvh := f.Section(".note.go.pvh")
	if pvh != nil && pvh.Addr > 0 {
		binVMAStart = pvh.Addr
	}

	for _, sec := range f.Sections {
		if sec.Addr == 0 || sec.Size == 0 {
			continue
		}
		if strings.HasPrefix(sec.Name, ".debug") ||
			sec.Name == ".symtab" || sec.Name == ".strtab" || sec.Name == ".shstrtab" {
			continue
		}
		if sec.Addr < binVMAStart && pvh == nil {
			binVMAStart = sec.Addr
		}
		end := sec.Addr + sec.Size
		if sec.Type == elf.SHT_NOBITS {
			if end > bssEnd {
				bssEnd = end
			}
		}
		if end > dataEnd {
			dataEnd = end
		}
	}

	binEnd := bssEnd
	if dataEnd > binEnd {
		binEnd = dataEnd
	}

	ca35Load := mcuToCA35(a35LoadAddr)
	entry := f.Entry
	var expectedEntryOffset uint64
	if entry > 0 {
		expectedEntryOffset = entry - binVMAStart
	}
	stackAddr := ramStart + ramSize - ramStackOffset
	heapStart := bssEnd
	heapSize := uint64(0)
	if stackAddr > heapStart {
		heapSize = stackAddr - heapStart
	}
	sspCA35 := mcuToCA35(sspLoad)
	tspCA35 := mcuToCA35(tspLoad)

	fmt.Printf("=== AST2700 DRAM Layout Validation ===\n")
	fmt.Printf("ELF:              %s\n", elfPath)
	if entry > 0 {
		fmt.Printf("Entry point:      0x%X\n", entry)
	} else {
		fmt.Printf("Entry point:      NOT FOUND\n")
	}
	fmt.Printf("Binary VMA:       [0x%X, 0x%X) (%.1f MB)\n",
		binVMAStart, binEnd, float64(binEnd-binVMAStart)/(1024*1024))
	fmt.Printf("CA35 load addr:   0x%X (BootMCU 0x%X)\n", ca35Load, a35LoadAddr)
	fmt.Println()
	fmt.Printf("RamStart:         0x%X\n", ramStart)
	fmt.Printf("RamSize:          0x%X (%.0f MB)\n", ramSize, float64(ramSize)/(1024*1024))
	fmt.Printf("Stack (SP):       0x%X\n", stackAddr)
	fmt.Printf("Heap:             [0x%X, 0x%X) (%.0f MB)\n",
		heapStart, stackAddr, float64(heapSize)/(1024*1024))
	fmt.Println()
	fmt.Printf("SSP (CA35):       [0x%X, 0x%X)\n", sspCA35, sspCA35+sspSize)
	fmt.Printf("TSP (CA35):       [0x%X, 0x%X)\n", tspCA35, tspCA35+tspSize)
	fmt.Println()

	var errors, warnings []string

	if binVMAStart != ca35Load {
		errors = append(errors, fmt.Sprintf(
			"Binary VMA start (0x%X) != CA35 load address (0x%X). "+
				"BootMCU A35_LOAD_ADDR should be 0x%X.",
			binVMAStart, ca35Load, ca35ToMCU(binVMAStart)))
	}

	if hasEntryOffset && entry > 0 && a35EntryOffset != expectedEntryOffset {
		errors = append(errors, fmt.Sprintf(
			"A35_ENTRY_RAW_OFFSET (0x%X) != actual offset (0x%X). Update BootMCU constant.",
			a35EntryOffset, expectedEntryOffset))
	}

	if binVMAStart < ramStart {
		errors = append(errors, fmt.Sprintf(
			"Binary starts at 0x%X, below RamStart 0x%X.", binVMAStart, ramStart))
	}
	if binEnd > ramStart+ramSize {
		errors = append(errors, fmt.Sprintf(
			"Binary ends at 0x%X, beyond RamEnd 0x%X.", binEnd, ramStart+ramSize))
	}

	const minHeapMB = 60
	if heapSize < minHeapMB*1024*1024 {
		errors = append(errors, fmt.Sprintf(
			"Heap space is only %.0f MB (need >= %d MB for Go page allocator). "+
				"Lower the linker -T address or increase RamSize.",
			float64(heapSize)/(1024*1024), minHeapMB))
	}

	if stackAddr > ramStart+ramSize {
		errors = append(errors, fmt.Sprintf("Stack 0x%X is beyond RamEnd.", stackAddr))
	}
	if stackAddr < binEnd {
		errors = append(errors, fmt.Sprintf("Stack 0x%X is inside the binary (ends 0x%X).", stackAddr, binEnd))
	}

	if overlaps(sspCA35, sspCA35+sspSize, binVMAStart, binEnd) {
		errors = append(errors, "SSP region overlaps TamaGo binary!")
	}
	if overlaps(tspCA35, tspCA35+tspSize, binVMAStart, binEnd) {
		errors = append(errors, "TSP region overlaps TamaGo binary!")
	}

	if overlaps(sspCA35, sspCA35+sspSize, heapStart, stackAddr) {
		warnings = append(warnings, "SSP region overlaps heap range (may be overwritten by Go allocator).")
	}
	if overlaps(tspCA35, tspCA35+tspSize, heapStart, stackAddr) {
		warnings = append(warnings, "TSP region overlaps heap range (may be overwritten by Go allocator).")
	}

	ptStart := ramStart + 0x4000
	ptEnd := ramStart + 0x8000
	if overlaps(ptStart, ptEnd, binVMAStart, binEnd) {
		errors = append(errors, fmt.Sprintf("MMU page tables [0x%X,0x%X) overlap binary!", ptStart, ptEnd))
	}

	for _, w := range warnings {
		fmt.Printf("  WARNING: %s\n", w)
	}
	for _, e := range errors {
		fmt.Printf("  ERROR: %s\n", e)
	}

	if len(errors) > 0 {
		fmt.Printf("\nFAILED: %d error(s), %d warning(s)\n", len(errors), len(warnings))
		return fmt.Errorf("validation failed")
	}
	fmt.Printf("PASSED (%d warning(s))\n", len(warnings))
	return nil
}

func mcuToCA35(mcuAddr uint64) uint64 {
	return (mcuAddr & 0x7FFFFFFF) | 0x400000000
}

func ca35ToMCU(ca35Addr uint64) uint64 {
	return (ca35Addr & 0x7FFFFFFF) | 0x80000000
}

func overlaps(aStart, aEnd, bStart, bEnd uint64) bool {
	return aStart < bEnd && bStart < aEnd
}

func parseHexAddr(s string) (uint64, error) {
	return parseSize(s)
}
