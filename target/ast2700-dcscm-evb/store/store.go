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

// RegionSize is the size of the dedicated Scree volume carved from the top of
// the FMC NOR. It is kept small and fixed for two reasons:
//
//   - The store lives on the SAME chip the firmware boots from. It MUST NOT
//     overlap the boot image (which occupies the low part of the NOR), so the
//     volume is placed in a dedicated region at the top of the device. See
//     regionBase in Open.
//   - Formatting erases every journal block one 4 KB sector at a time. On the
//     full 64 MB chip that is ~16k sequential erases (minutes). A 4 MB region
//     is ~1k erases, and it matches the management-plane RAM store budget.
const RegionSize = 4 << 20

// Open initialises the FMC SPI NOR and returns a Scree block device backed by a
// dedicated region at the top of the chip. size is the NOR capacity in bytes
// (use DefaultNORSize for the DC-SCM board).
//
// The volume is mounted if a valid one is already present; only a fresh or
// corrupt region is (re)formatted. This persists state across reboots and,
// critically, avoids re-erasing the region on every boot.
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

	// Carve the store region from the top of the NOR, safely above the boot
	// image. The store must never touch the firmware region or a cold reboot
	// would find a corrupted image and fail to load the CA35 payload.
	regionSize := int64(RegionSize)
	if regionSize > size {
		regionSize = size
	}
	regionBase := size - regionSize

	dev, err := blkdev.NewNOR(screeNOR{nor: nor, base: regionBase, size: regionSize})
	if err != nil {
		return nil, fmt.Errorf("create Scree NOR block device: %w", err)
	}

	// Mount an existing volume when present; only format a fresh/corrupt region.
	if st, err := scree.Mount(dev, scree.MountOptions{}); err == nil {
		_ = st.Close()
		return dev, nil
	}
	if err := scree.Format(dev, scree.FormatOptions{JournalBlocks: dev.BlockCount() - 4}); err != nil {
		return nil, fmt.Errorf("format Scree: %w", err)
	}
	return dev, nil
}

// screeNOR adapts the aspeed-go spinor.NOR to Scree's driver.NORFlash and
// confines the device to a [base, base+size) window of the underlying chip, so
// the Scree volume occupies a dedicated NOR region that never overlaps the
// firmware image.
type screeNOR struct {
	nor  *spinor.NOR
	base int64
	size int64
}

func (n screeNOR) Geometry() screedriver.Geometry {
	g := n.nor.Geometry()
	return screedriver.Geometry{
		TotalSize:      n.size,
		EraseBlockSize: g.EraseBlockSize,
		PageSize:       g.PageSize,
		EraseValue:     g.EraseValue,
	}
}

func (n screeNOR) ReadAt(dst []byte, addr int64) (int, error) {
	return n.nor.ReadAt(dst, n.base+addr)
}

func (n screeNOR) WriteAt(src []byte, addr int64) (int, error) {
	return n.nor.WriteAt(src, n.base+addr)
}

func (n screeNOR) EraseBlock(addr int64) error {
	return n.nor.EraseBlock(n.base + addr)
}
