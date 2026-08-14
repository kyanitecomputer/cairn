// Package store provides the flash-backed Scree volumes for the AST2700 DC-SCM
// board on the FMC-attached SPI NOR:
//
//   - Open carves the JetStream backing store from the top of the chip.
//   - OpenConfig carves a smaller, persistent config/TLS volume just below it.
//
// Both volumes share a single SPI controller and one spinor.NOR (whose mutex
// serializes bus access), so they can live on disjoint regions of the same chip
// without corrupting each other. core/natscore owns the JetStream store-type
// registration; core/cfgstore owns the config KV binding.
//
// This package is only compiled for GOOS=tamago.

//go:build tamago

package store

import (
	"fmt"
	"log/slog"
	"sync"

	"src.kyanite.computer/aspeed-go/hal/spi"
	"src.kyanite.computer/aspeed-go/hal/spinor"
	"src.kyanite.computer/core/cfgstore"
	"src.kyanite.computer/scree"
	"src.kyanite.computer/scree/blkdev"
	screedriver "src.kyanite.computer/scree/driver"
)

// DefaultNORSize is the SPI NOR size on the AST2700 DC-SCM board (64 MB).
const DefaultNORSize = 64 << 20

// RegionSize is the size of the dedicated JetStream Scree volume carved from the
// top of the FMC NOR. It is kept small and fixed for two reasons:
//
//   - The store lives on the SAME chip the firmware boots from. It MUST NOT
//     overlap the boot image (which occupies the low part of the NOR), so the
//     volume is placed in a dedicated region at the top of the device.
//   - Formatting erases every journal block one 4 KB sector at a time. On the
//     full 64 MB chip that is ~16k sequential erases (minutes). A 4 MB region
//     is ~1k erases, and it matches the management-plane RAM store budget.
const RegionSize = 4 << 20

// ConfigRegionSize is the persistent config/TLS volume, carved just below the
// JetStream region. Small: it holds a handful of config keys plus the web TLS
// certificate/key.
const ConfigRegionSize = 2 << 20

// configPageSize is the config volume's logical write unit (Scree journal
// record size). Scree stores each metadata record in one write unit
// (journal.append rejects anything larger), so this must comfortably exceed a
// PEM-encoded TLS certificate — a self-signed ECDSA cert is ~650 bytes, well
// over the 512-byte default. 4 KB (one erase block) leaves ample headroom for
// operator-uploaded certificates too. The physical NOR still programs 256-byte
// pages; spinor.WriteAt splits the larger logical writes internally.
const configPageSize = 4096

// A single controller + NOR shared by every volume on the chip. spinor.NOR's
// mutex makes concurrent access from the JetStream and config volumes safe.
var (
	busOnce sync.Once
	busNOR  *spinor.NOR
	busErr  error
)

func openBus(size int64) (*spinor.NOR, error) {
	busOnce.Do(func() {
		ctl := &spi.Controller{Base: spi.AST2700FMCBase, WindowBase: spi.AST2700FMCWindow, MaxCS: 3, SoC: spi.AST2700}
		if err := ctl.Init(); err != nil {
			busErr = fmt.Errorf("init FMC: %w", err)
			return
		}
		bus, err := ctl.Device(0)
		if err != nil {
			busErr = fmt.Errorf("open FMC CE0: %w", err)
			return
		}
		nor := &spinor.NOR{Bus: bus, Size: size, PageSize: 256, EraseSize: 4096}
		if err := nor.Init(); err != nil {
			busErr = fmt.Errorf("init SPI NOR: %w", err)
			return
		}
		id := nor.ReadID()
		slog.Info("store: FMC SPI NOR detected",
			"jedec_id", fmt.Sprintf("%02x %02x %02x", id[0], id[1], id[2]),
			"size", size)
		busNOR = nor
	})
	return busNOR, busErr
}

// Open initialises the FMC SPI NOR and returns a Scree block device backed by
// the JetStream region at the top of the chip. size is the NOR capacity in
// bytes (use DefaultNORSize for the DC-SCM board).
//
// The volume is mounted if a valid one is already present; only a fresh or
// corrupt region is (re)formatted, so reboots do not re-erase the region and
// state persists across boots.
func Open(size int64) (blkdev.BlockDevice, error) {
	nor, err := openBus(size)
	if err != nil {
		return nil, err
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
	st, mErr := scree.Mount(dev, scree.MountOptions{})
	if mErr == nil {
		_ = st.Close()
		slog.Info("store: mounted existing Scree volume (persisted across reboot)",
			"region_base", fmt.Sprintf("%#x", regionBase), "region_size", regionSize)
		return dev, nil
	}
	slog.Info("store: no valid Scree volume — formatting fresh region",
		"region_base", fmt.Sprintf("%#x", regionBase), "region_size", regionSize,
		"mount_err", mErr)
	if err := scree.Format(dev, scree.FormatOptions{JournalBlocks: dev.BlockCount() - 4}); err != nil {
		return nil, fmt.Errorf("format Scree: %w", err)
	}
	slog.Info("store: formatted fresh Scree volume", "journal_blocks", dev.BlockCount()-4)
	return dev, nil
}

// OpenConfig initialises the persistent config/TLS Scree volume on a dedicated
// region just below the JetStream region, and returns a cfgstore.Store bound to
// its "config" namespace. It survives reboots, so the web TLS certificate and
// system configuration persist. size is the NOR capacity in bytes.
func OpenConfig(size int64) (*cfgstore.Store, error) {
	nor, err := openBus(size)
	if err != nil {
		return nil, err
	}
	base := size - int64(RegionSize) - int64(ConfigRegionSize)
	if base < 0 {
		return nil, fmt.Errorf("store: config region does not fit in %d-byte NOR", size)
	}

	dev, err := blkdev.NewNOR(screeNOR{nor: nor, base: base, size: ConfigRegionSize, pageSize: configPageSize})
	if err != nil {
		return nil, fmt.Errorf("create config Scree NOR block device: %w", err)
	}
	// Reserve the four super/master blocks; give the rest to the journal.
	st, err := cfgstore.Open(dev, dev.BlockCount()-4)
	if err != nil {
		return nil, fmt.Errorf("open config Scree: %w", err)
	}
	slog.Info("store: config volume ready (persistent TLS/config on FMC SPI NOR)",
		"region_base", fmt.Sprintf("%#x", base), "region_size", ConfigRegionSize)
	return st, nil
}

// screeNOR adapts the aspeed-go spinor.NOR to Scree's driver.NORFlash and
// confines the device to a [base, base+size) window of the underlying chip, so
// each Scree volume occupies a dedicated NOR region that never overlaps another
// or the firmware image.
type screeNOR struct {
	nor  *spinor.NOR
	base int64
	size int64
	// pageSize overrides the logical write-unit (Scree record size) for this
	// volume; 0 uses the chip's physical page size.
	pageSize int
}

func (n screeNOR) Geometry() screedriver.Geometry {
	g := n.nor.Geometry()
	page := g.PageSize
	if n.pageSize != 0 {
		page = n.pageSize
	}
	return screedriver.Geometry{
		TotalSize:      n.size,
		EraseBlockSize: g.EraseBlockSize,
		PageSize:       page,
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
