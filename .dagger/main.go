// Dagger CI module for Cairn — the Kyanite baseboard-management software stack.
//
// Builds use a single StageX image (reproducible, minimal) that provides the Go
// toolchain. The Go runtime is cross-compiled with the TamaGo compiler
// (GOOS=tamago GOARCH=arm64). The ASPEED AST2700 arm64 SoC/board support lives
// in a TamaGo fork that is not yet upstream, so the fork and the tamago-go
// compiler are passed in as directories and wired up with an in-container
// go.work alongside the shared core module.
//
// The AST2700 boots via its BootMCU (RISC-V RV32), which prepares DRAM and loads
// the CA35 payload. The `image` function builds that BootMCU firmware from the
// aspeed-mcu-runtime Rust sources, builds the cairn CA35 payload, and stitches a
// complete SPI flash image with imgtools (in tools/imgtools) using the bmc-pb
// prebuilts (Caliptra, DDR training, DP FW) for the selected silicon revision.
// The image-stitching logic lives here in cairn; aspeed-mcu-runtime is consumed
// only as the Rust runtime source.
//
// NOTE: the generated Dagger SDK glue (dagger.gen.go, internal/dagger) is not
// committed; run `dagger develop` once to regenerate it before invoking these
// functions.
//
// Usage (from the cairn repo root). Common inputs:
//
//	CORE="--tamago ../tamago --tamago-go ../../tamago/tamago-go --core ../core --scree ../scree --nats-server ../nats-server --aspeed-go ../aspeed-go --lneto ../lneto"
//
//	dagger call build $CORE
//	dagger call test  $CORE
//	dagger call ci    $CORE
//	dagger call image $CORE \
//	    --aspeed-mcu-runtime ../aspeed-mcu-runtime --aspeed-rs ../aspeed-rs \
//	    --aspeed-data ../aspeed-data --bmc-pb ../bmc-pb \
//	    --silicon a2 export --path ./out
package main

import (
	"context"
	"fmt"

	"dagger/cairn/internal/dagger"
)

const (
	// StageX build images. pallet-cgo (Docker Hub) provides go + clang + lld +
	// llvm-*; the Rust image supplies the BootMCU cross toolchain.
	stagexPallet = "stagex/pallet-cgo:sx2026.06.0"
	stagexRust   = "stagex/pallet-rust:sx2026.06.0"

	// CA35-view DRAM text base. The payload links 64 MB into DRAM
	// (RamStart 0x400000000 + 0x4000000), leaving ~960 MB of heap between the
	// binary end and the stack — the layout proven by the cmd/nats validation.
	runtimeText = "0x404000000"
	roAlign     = "0x1000"

	// defaultBuildTags select the AST2700 DC-SCM board (linkcpuinit brings up
	// the CA35 secondary-init path). Override for other silicon/board variants.
	defaultBuildTags = "linkcpuinit,ast2700dcscm"

	// BootMCU (RISC-V RV32) firmware, built from the aspeed-mcu-runtime Rust
	// sources. These match aspeed-mcu-runtime's own build parameters.
	rustChannel   = "nightly-2026-04-01"
	bootmcuTarget = "riscv32imc-unknown-none-elf"
	bootmcuBin    = "rot_ast2700_bootmcu"
	bootmcuFlags  = "--cfg portable_atomic_unsafe_assume_single_core -C link-arg=-Tmemory.x -C link-arg=-Tlink.x -C link-arg=--nmagic"
)

// Cairn is the Dagger module root.
type Cairn struct{}

// base composes the StageX build container with the tamago-go compiler, the
// ASPEED tamago fork, and the shared core module wired up through an
// in-container go.work. The tamago-go binary is placed first on PATH so `go`
// resolves to the TamaGo compiler.
func (m *Cairn) base(
	src *dagger.Directory,
	tamago *dagger.Directory,
	tamagoGo *dagger.Directory,
	core *dagger.Directory,
	scree *dagger.Directory,
	natsServer *dagger.Directory,
	aspeedGo *dagger.Directory,
	lneto *dagger.Directory,
) *dagger.Container {
	goCache := dag.CacheVolume("cairn-go-mod")
	goBuild := dag.CacheVolume("cairn-go-build")

	// In-container workspace: cairn + the shared core module + the tamago fork
	// (AST2700 support) + scree + nats-server + aspeed-go (FTGMAC/SPI HAL) +
	// lneto (userspace TCP/IP).
	goWork := "go 1.26.4\n\nuse (\n\t./cairn\n\t./core\n\t./tamago\n\t./scree\n\t./nats-server\n\t./aspeed-go\n\t./lneto\n)\n"

	return dag.Container().
		From(stagexPallet).
		WithMountedCache("/go/pkg/mod", goCache).
		WithMountedCache("/root/.cache/go-build", goBuild).
		WithDirectory("/build/cairn", src).
		WithDirectory("/build/core", core).
		WithDirectory("/build/tamago", tamago).
		WithDirectory("/build/tamago-go", tamagoGo).
		WithDirectory("/build/scree", scree).
		WithDirectory("/build/nats-server", natsServer).
		WithDirectory("/build/aspeed-go", aspeedGo).
		WithDirectory("/build/lneto", lneto).
		WithNewFile("/build/go.work", goWork).
		WithEnvVariable("GOWORK", "/build/go.work").
		WithEnvVariable("GOTOOLCHAIN", "local").
		WithEnvVariable("GOOS", "tamago").
		WithEnvVariable("GOARCH", "arm64").
		WithEnvVariable("GOOSPKG", "github.com/usbarmory/tamago").
		WithEnvVariable("PATH", "/build/tamago-go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin").
		WithWorkdir("/build/cairn")
}

// elf cross-compiles the AST2700 DC-SCM EVB ELF into /out/cairn.elf.
func (m *Cairn) elf(src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto *dagger.Directory) *dagger.Container {
	return m.base(src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto).
		WithExec([]string{"mkdir", "-p", "/out"}).
		WithExec([]string{
			"go", "build", "-trimpath",
			"-tags", defaultBuildTags,
			"-ldflags", "-T " + runtimeText + " -R " + roAlign,
			"-o", "/out/cairn.elf",
			"./target/ast2700-dcscm-evb",
		})
}

// Build cross-compiles the AST2700 DC-SCM EVB ELF and returns it.
func (m *Cairn) Build(
	// +defaultPath="."
	src *dagger.Directory,
	tamago *dagger.Directory,
	tamagoGo *dagger.Directory,
	core *dagger.Directory,
	scree *dagger.Directory,
	natsServer *dagger.Directory,
	aspeedGo *dagger.Directory,
	lneto *dagger.Directory,
) *dagger.File {
	return m.elf(src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto).File("/out/cairn.elf")
}

// Test runs the host unit tests (Linux userspace, user_linux build tag).
func (m *Cairn) Test(
	ctx context.Context,
	// +defaultPath="."
	src *dagger.Directory,
	tamago *dagger.Directory,
	tamagoGo *dagger.Directory,
	core *dagger.Directory,
	scree *dagger.Directory,
	natsServer *dagger.Directory,
	aspeedGo *dagger.Directory,
	lneto *dagger.Directory,
) (string, error) {
	return m.base(src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto).
		WithEnvVariable("GOOS", "linux").
		WithEnvVariable("GOARCH", "amd64").
		WithExec([]string{"go", "test", "-tags", "user_linux", "./..."}).
		Stdout(ctx)
}

// imageContainer composes the cairn payload build (Go/TamaGo) with the imgtools
// stitcher (host Go) and the bmc-pb prebuilts — everything needed to stitch an
// AST2700 SPI flash image except the BootMCU firmware, which is either provided
// as a prebuilt (see Image's bootMcuFmc) or built from Rust by bootMCU below.
func (m *Cairn) imageContainer(
	src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto, bmcPb *dagger.Directory,
) *dagger.Container {
	return m.base(src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto).
		WithDirectory("/build/bmc-pb", bmcPb).
		WithExec([]string{"mkdir", "-p", "/out"}).
		// Build the imgtools stitcher as a host (linux/amd64) binary — it runs
		// in-container to stitch the image, so it must not inherit the payload's
		// GOOS=tamago. Restore the tamago cross-build env afterwards.
		WithEnvVariable("GOWORK", "off").
		WithEnvVariable("GOOS", "linux").
		WithEnvVariable("GOARCH", "amd64").
		WithExec([]string{"go", "build", "-C", "/build/cairn/tools/imgtools", "-o", "/usr/local/bin/imgtools", "."}).
		WithEnvVariable("GOWORK", "/build/go.work").
		WithEnvVariable("GOOS", "tamago").
		WithEnvVariable("GOARCH", "arm64")
}

// bootMCU builds the BootMCU firmware (Rust) from the aspeed-mcu-runtime sources
// and writes the raw FMC binary to /out/rot_ast2700_bootmcu.fmc.bin. The Rust
// nightly toolchain (with the RV32 target) is composed in from the StageX Rust
// image; the image reference is pinned/overridable via rustImage.
func (m *Cairn) bootMCU(
	ctr *dagger.Container,
	rustImage string,
	aspeedMcuRuntime, aspeedRs, aspeedData *dagger.Directory,
) *dagger.Container {
	rustCtr := dag.Container().
		From(rustImage).
		WithExec([]string{
			"rustup", "toolchain", "install", rustChannel,
			"--profile", "minimal",
			"--target", bootmcuTarget,
			"--no-self-update",
		}).
		WithExec([]string{"rustup", "default", rustChannel})

	cargoCache := dag.CacheVolume("cairn-cargo-registry")
	cargoBuild := dag.CacheVolume("cairn-cargo-build")

	return ctr.
		WithDirectory("/usr/local/cargo", rustCtr.Directory("/usr/local/cargo")).
		WithDirectory("/usr/local/rustup", rustCtr.Directory("/usr/local/rustup")).
		WithEnvVariable("CARGO_HOME", "/usr/local/cargo").
		WithEnvVariable("RUSTUP_HOME", "/usr/local/rustup").
		WithEnvVariable("PATH", "/build/tamago-go/bin:/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin").
		WithMountedCache("/usr/local/cargo/registry", cargoCache).
		WithMountedCache("/build/aspeed-mcu-runtime/aspeed-mcu-target", cargoBuild).
		WithDirectory("/build/aspeed-mcu-runtime", aspeedMcuRuntime).
		WithDirectory("/build/aspeed-rs", aspeedRs).
		WithDirectory("/build/aspeed-data", aspeedData).
		WithWorkdir("/build/aspeed-mcu-runtime/app-rot").
		WithEnvVariable("RUSTFLAGS", bootmcuFlags).
		WithExec([]string{"cargo", "build",
			"--target-dir", "/build/aspeed-mcu-runtime/aspeed-mcu-target",
			"--target", bootmcuTarget,
			"--release",
			"--bin", bootmcuBin,
			"--no-default-features",
			"--features", "ast2700-bootmcu",
		}).
		WithExec([]string{"llvm-objcopy", "-O", "binary",
			"/build/aspeed-mcu-runtime/aspeed-mcu-target/" + bootmcuTarget + "/release/" + bootmcuBin,
			"/out/rot_ast2700_bootmcu.fmc.bin"}).
		WithWorkdir("/build/cairn")
}

// Image builds the cairn CA35 payload (TamaGo) and stitches it, the BootMCU
// firmware, and the bmc-pb prebuilts (Caliptra, DDR training, DP FW) into a
// complete AST2700 SPI flash image using imgtools.
//
// The BootMCU firmware is either supplied prebuilt via bootMcuFmc (skips the
// Rust build — useful where the StageX Rust image is unavailable) or built from
// the aspeed-mcu-runtime Rust sources (requires aspeedMcuRuntime/aspeedRs/
// aspeedData and a rustup-capable, musl-compatible rustImage).
//
// silicon selects the bmc-pb subdirectory (a1 → ast2700a1, a2 → ast2700a2). The
// BootMCU detects the silicon revision at runtime, so a single firmware serves
// both; silicon only selects the DRAM-training/Caliptra/DP prebuilt set.
// buildTags overrides the payload build tags for board/silicon variants.
func (m *Cairn) Image(
	ctx context.Context,
	// +defaultPath="."
	src *dagger.Directory,
	tamago *dagger.Directory,
	tamagoGo *dagger.Directory,
	core *dagger.Directory,
	scree *dagger.Directory,
	natsServer *dagger.Directory,
	aspeedGo *dagger.Directory,
	lneto *dagger.Directory,
	bmcPb *dagger.Directory,
	// +optional
	bootMcuFmc *dagger.File,
	// +optional
	aspeedMcuRuntime *dagger.Directory,
	// +optional
	aspeedRs *dagger.Directory,
	// +optional
	aspeedData *dagger.Directory,
	// +default="stagex/pallet-rust:sx2026.06.0"
	rustImage string,
	// +default="a1"
	silicon string,
	// +default="32M"
	imageSize string,
	// +default="cairn_ast2700.bin"
	outputName string,
	// +default=""
	buildTags string,
) (*dagger.Directory, error) {
	if rustImage == "" {
		rustImage = stagexRust
	}
	if silicon == "" {
		silicon = "a1"
	}
	if imageSize == "" {
		imageSize = "32M"
	}
	if outputName == "" {
		outputName = "cairn_ast2700.bin"
	}
	if buildTags == "" {
		buildTags = defaultBuildTags
	}
	pb := "/build/bmc-pb/ast2700" + silicon

	ctr := m.imageContainer(src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto, bmcPb)

	// BootMCU firmware: use the prebuilt FMC when provided, else build from Rust.
	if bootMcuFmc != nil {
		ctr = ctr.WithFile("/out/rot_ast2700_bootmcu.fmc.bin", bootMcuFmc)
	} else {
		if aspeedMcuRuntime == nil || aspeedRs == nil || aspeedData == nil {
			return nil, fmt.Errorf("image: building the BootMCU needs --aspeed-mcu-runtime, --aspeed-rs and --aspeed-data (or supply --boot-mcu-fmc)")
		}
		ctr = m.bootMCU(ctr, rustImage, aspeedMcuRuntime, aspeedRs, aspeedData)
	}

	ctr = ctr.
		// cairn CA35 payload (TamaGo) → raw binary.
		WithWorkdir("/build/cairn").
		WithExec([]string{"go", "build", "-trimpath",
			"-tags", buildTags,
			"-ldflags", "-T " + runtimeText + " -R " + roAlign,
			"-o", "/out/cairn.elf",
			"./target/ast2700-dcscm-evb",
		}).
		WithExec([]string{"llvm-objcopy", "-O", "binary", "/out/cairn.elf", "/out/cairn.raw.bin"}).
		// Stitch the SPI flash image.
		WithExec([]string{
			"imgtools", "spi-image",
			"--caliptra", pb + "/caliptra-fw.bin",
			"--fmc", "/out/rot_ast2700_bootmcu.fmc.bin",
			"--prebuilt", "1:" + pb + "/ddr4_pmu_train_imem.bin",
			"--prebuilt", "2:" + pb + "/ddr4_pmu_train_dmem.bin",
			"--prebuilt", "3:" + pb + "/ddr4_2d_pmu_train_imem.bin",
			"--prebuilt", "4:" + pb + "/ddr4_2d_pmu_train_dmem.bin",
			"--prebuilt", "5:" + pb + "/ddr5_pmu_train_imem.bin",
			"--prebuilt", "6:" + pb + "/ddr5_pmu_train_dmem.bin",
			"--prebuilt", "7:" + pb + "/dp_fw.bin",
			"--psp-payload", "/out/cairn.raw.bin",
			"--psp-elf", "/out/cairn.elf",
			"--size", imageSize,
			"--output", "/out/" + outputName,
		})

	return ctr.Directory("/out").Sync(ctx)
}

// Ci runs the full pipeline: build the ELF and run host unit tests.
func (m *Cairn) Ci(
	ctx context.Context,
	// +defaultPath="."
	src *dagger.Directory,
	tamago *dagger.Directory,
	tamagoGo *dagger.Directory,
	core *dagger.Directory,
	scree *dagger.Directory,
	natsServer *dagger.Directory,
	aspeedGo *dagger.Directory,
	lneto *dagger.Directory,
) (string, error) {
	if _, err := m.elf(src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto).Sync(ctx); err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	if _, err := m.Test(ctx, src, tamago, tamagoGo, core, scree, natsServer, aspeedGo, lneto); err != nil {
		return "", fmt.Errorf("test: %w", err)
	}
	return "build: ok\ntest: ok", nil
}
