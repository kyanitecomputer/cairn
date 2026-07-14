package main

import (
	"flag"
	"fmt"
	"os"
)

func cmdFlashImage(args []string) error {
	fs := flag.NewFlagSet("flash-image", flag.ExitOnError)
	var input, output string
	var size int
	fs.StringVar(&input, "input", "", "Input firmware binary")
	fs.StringVar(&output, "output", "", "Output flash image")
	fs.IntVar(&size, "size", 1048576, "Target flash size in bytes (default 1MB)")
	fs.Parse(args)

	if input == "" || output == "" {
		fs.Usage()
		return fmt.Errorf("--input and --output are required")
	}

	data, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read %s: %w", input, err)
	}
	if len(data) > size {
		return fmt.Errorf("input (%d bytes) exceeds target size (%d bytes)", len(data), size)
	}

	out := make([]byte, size)
	copy(out, data)

	if err := os.WriteFile(output, out, 0644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}

	fmt.Printf("Flash image: %s (%d bytes, firmware: %d bytes, padding: %d bytes)\n",
		output, size, len(data), size-len(data))
	return nil
}
