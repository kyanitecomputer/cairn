package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

func cmdUartImage(args []string) error {
	fs := flag.NewFlagSet("uart-image", flag.ExitOnError)
	var input, output string
	fs.StringVar(&input, "input", "", "Input firmware binary")
	fs.StringVar(&output, "output", "", "Output UART boot image")
	fs.Parse(args)

	if input == "" || output == "" {
		fs.Usage()
		return fmt.Errorf("--input and --output are required")
	}

	data, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read %s: %w", input, err)
	}

	alignedSize := alignUp(uint64(len(data)), 4)
	out := make([]byte, 4+alignedSize)
	binary.LittleEndian.PutUint32(out[0:4], uint32(alignedSize))
	copy(out[4:], data)

	if err := os.WriteFile(output, out, 0644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}

	fmt.Printf("UART boot image: %s (header: 4 bytes, payload: %d bytes, aligned: %d bytes, total: %d bytes)\n",
		output, len(data), alignedSize, len(out))
	return nil
}
