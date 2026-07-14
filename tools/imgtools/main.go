package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "flash-image":
		err = cmdFlashImage(os.Args[2:])
	case "uart-image":
		err = cmdUartImage(os.Args[2:])
	case "fmc-image":
		err = cmdFmcImage(os.Args[2:])
	case "spi-image":
		err = cmdSpiImage(os.Args[2:])
	case "validate-layout":
		err = cmdValidateLayout(os.Args[2:])
	case "secure-image":
		err = cmdSecureImage(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: imgtools <command> [flags]

Commands:
  flash-image       Pad binary to SPI flash size
  uart-image        Add UART boot header
  fmc-image         Generate ASTH header + FMC image (AST2700)
  spi-image         Generate AST2700 SPI flash image
  validate-layout   Validate DRAM memory layout for TamaGo payloads
  secure-image      Generate signed/encrypted boot image (AST2600/AST10x0)`)
}
