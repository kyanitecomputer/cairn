// Package store provides the flash-backed Scree volume for the AST2700 DC-SCM
// board: a Scree filesystem on the FMC-attached SPI NOR, used as the persistent
// JetStream backing store for the management plane.
//
// Ported from the cmd/nats validation harness (storage.go). Unlike that harness
// it does not register the JetStream store type itself — core/natscore owns the
// single ScreeStorage registration — it only builds and formats the block
// device, which the caller hands to natscore.
//
// This package is only compiled for GOOS=tamago.

//go:build tamago

package store

import (
	"fmt"

	"github.com/kyanitecomputer/aspeed-go/hal/spi"
	"github.com/kyanitecomputer/aspeed-go/hal/spinor"
	"src.kyanite.computer/scree"
	"src.kyanite.computer/scree/blkdev"
	screedriver "src.kyanite.computer/scree/driver"
)

// DefaultNORSize is the SPI NOR size on the AST2700 DC-SCM board (64 MB).
const DefaultNORSize = 64 << 20

// Open initialises the FMC SPI NOR, wraps it in a Scree block device, formats a
// fresh Scree volume, and returns the device. size is the NOR capacity in bytes
// (use DefaultNORSize for the DC-SCM board).
//
// The volume is reformatted on every call; persistence across reboot is a later
// step (mount-if-present rather than always-format).
func Open(size int64) (blkdev.BlockDevice, error) {
	ctl := &spi.Controller{Base: spi.AST2700FMCBase, WindowBase: spi.AST2700FMCWindow, MaxCS: 3}
	if err := ctl.Init(); err != nil {
		return nil, fmt.Errorf("init FMC: %w", err)
	}
	bus, err := ctl.Device(0)
	if err != nil {
		return nil, fmt.Errorf("open FMC CE0: %w", err)
	}
	nor := &spinor.NOR{Bus: bus, Size: size, PageSize: 256, EraseSize: 4096}
	if err := nor.Init(); err != nil {
		return nil, fmt.Errorf("init SPI NOR: %w", err)
	}
	dev, err := blkdev.NewNOR(screeNOR{nor})
	if err != nil {
		return nil, fmt.Errorf("create Scree NOR block device: %w", err)
	}
	if err := scree.Format(dev, scree.FormatOptions{JournalBlocks: dev.BlockCount() - 4}); err != nil {
		return nil, fmt.Errorf("format Scree: %w", err)
	}
	return dev, nil
}

// screeNOR adapts the aspeed-go spinor.NOR geometry to Scree's driver.Geometry.
type screeNOR struct{ *spinor.NOR }

func (n screeNOR) Geometry() screedriver.Geometry {
	g := n.NOR.Geometry()
	return screedriver.Geometry{
		TotalSize:      g.TotalSize,
		EraseBlockSize: g.EraseBlockSize,
		PageSize:       g.PageSize,
		EraseValue:     g.EraseValue,
	}
}
