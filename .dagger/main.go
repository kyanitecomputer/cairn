// Dagger CI module for Cairn — the Kyanite baseboard-management software stack.
//
// Builds use a single StageX image (reproducible, minimal). The Go runtime is
// cross-compiled with the TamaGo compiler (GOOS=tamago GOARCH=arm64), installed
// in-container from the upstream tamago-go release. The ASPEED AST2700 SoC/board
// support and the other Kyanite modules are fetched from public GitHub and wired
// up with an in-container go.work; every source directory defaults to its
// kyanitecomputer repository, so the whole flow builds from public sources with
// no local checkouts. Pass --<dir> to override any source for local development.
//
// The AST2700 boots via its BootMCU (RISC-V RV32), which prepares DRAM and loads
// the CA35 payload. The `image` function builds that BootMCU firmware from the
// mcu-runtime Rust sources, builds the cairn CA35 payload, and stitches a
// complete SPI flash image with imgtools (in tools/imgtools) using the bmc-pb
// prebuilts (Caliptra, DDR training, DP FW) for the selected silicon revision.
//
// NOTE: the generated Dagger SDK glue (dagger.gen.go, internal/dagger) is not
// committed; run `dagger develop` once to regenerate it before invoking these
// functions.
//
// Usage (from the cairn repo root) — no arguments needed:
//
//	dagger call build export --path ./bin              # CA35 payload ELF
//	dagger call test                                   # host unit tests
//	dagger call ci                                     # build + test
//	dagger call image --bmc-pb ../bmc-pb --silicon a1 export --path ./out
//
// Only the A1 image needs --bmc-pb (the non-redistributable vendor prebuilts).
// The AST2700-A2 image (FLSH container + signed ATMN SoC manifest) is built by
// tools/a2/gen-a2-image.sh, not by this dagger function — the manifest signing
// uses the vendor cptra_imgtool and is not reproducible in-container.
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

	// BootMCU (RISC-V RV32) firmware, built from the mcu-runtime Rust
	// sources. These match mcu-runtime's own build parameters.
	bootmcuTarget = "riscv32imc-unknown-none-elf"
	bootmcuBin    = "rot_ast2700_bootmcu"
	// No portable_atomic_unsafe_assume_single_core cfg: app-rot deliberately
	// uses portable-atomic's critical-section provider (riscv
	// critical-section-single-hart), which is mutually exclusive with that cfg.
	bootmcuFlags = "-C link-arg=-Tmemory.x -C link-arg=-Tlink.x -C link-arg=--nmagic"

	// Public source defaults. When a source directory is not passed explicitly,
	// it is fetched from the kyanitecomputer GitHub org so the whole flow builds
	// from public sources with no local checkouts. Override any --<dir> for
	// local development.
	orgGit        = "https://github.com/kyanitecomputer/"
	branchMain    = "main"
	tamagoRef     = "aspeed"        // tamago fork branch carrying AST2700 support
	natsServerRef = "fs-interface"  // nats-server fork branch (custom storage provider)
	lnetoRepo     = "https://github.com/soypat/lneto"
	lnetoRef      = "8407e075f12a4beaf0d64eda3dede1f8023644b8" // commit cairn builds against (provides x/netdev)

	// The TamaGo compiler is installed from the upstream release (no fork). The
	// linux-amd64 tarball unpacks to /usr/local/tamago-go with the go binary at
	// /usr/local/tamago-go/bin/go.
	tamagoGoURL = "https://github.com/usbarmory/tamago-go/releases/download/tamago-go1.27.1/tamago-go1.27.1.linux-amd64.tar.gz"
	tamagoGoBin = "/usr/local/tamago-go/bin"
)

// orRepo returns dir when supplied, otherwise the tree of a kyanitecomputer
// repository branch — the default that lets builds run from public sources.
func orRepo(dir *dagger.Directory, repo, ref string) *dagger.Directory {
	if dir != nil {
		return dir
	}
	return dag.Git(orgGit + repo).Branch(ref).Tree()
}

// Cairn is the Dagger module root.
type Cairn struct{}

// base composes the StageX build container with the tamago-go compiler, the
// ASPEED tamago fork, and the shared core module wired up through an
// in-container go.work. The tamago-go binary is placed first on PATH so `go`
// resolves to the TamaGo compiler.
func (m *Cairn) base(
	src *dagger.Directory,
	tamago *dagger.Directory,
	core *dagger.Directory,
	scree *dagger.Directory,
	natsServer *dagger.Directory,
	aspeedGo *dagger.Directory,
	lneto *dagger.Directory,
	schema *dagger.Directory,
) *dagger.Container {
	// Resolve any source not supplied by the caller from public GitHub.
	tamago = orRepo(tamago, "tamago", tamagoRef)
	core = orRepo(core, "core", branchMain)
	scree = orRepo(scree, "scree", branchMain)
	natsServer = orRepo(natsServer, "nats-server", natsServerRef)
	aspeedGo = orRepo(aspeedGo, "aspeed-go", branchMain)
	schema = orRepo(schema, "schema", branchMain)
	if lneto == nil {
		lneto = dag.Git(lnetoRepo).Commit(lnetoRef).Tree()
	}

	goCache := dag.CacheVolume("cairn-go-mod")
	goBuild := dag.CacheVolume("cairn-go-build")

	// In-container workspace: cairn + the shared core module + the tamago fork
	// (AST2700 support) + scree + nats-server + aspeed-go (FTGMAC/SPI HAL) +
	// lneto (userspace TCP/IP) + schema/gen/go (generated protobuf module,
	// vanity import src.kyanite.computer/schema/gen/go).
	goWork := "go 1.27\n\nuse (\n\t./cairn\n\t./core\n\t./tamago\n\t./scree\n\t./nats-server\n\t./aspeed-go\n\t./lneto\n\t./schema/gen/go\n)\n"

	return dag.Container().
		From(stagexPallet).
		WithMountedCache("/go/pkg/mod", goCache).
		WithMountedCache("/root/.cache/go-build", goBuild).
		// Install the TamaGo compiler from the upstream release tarball
		// (unpacks to /usr/local/tamago-go).
		WithMountedFile("/tmp/tamago-go.tar.gz", dag.HTTP(tamagoGoURL)).
		WithExec([]string{"tar", "-xzf", "/tmp/tamago-go.tar.gz", "-C", "/"}).
		WithDirectory("/build/cairn", src).
		WithDirectory("/build/core", core).
		WithDirectory("/build/tamago", tamago).
		WithDirectory("/build/scree", scree).
		WithDirectory("/build/nats-server", natsServer).
		WithDirectory("/build/aspeed-go", aspeedGo).
		WithDirectory("/build/lneto", lneto).
		WithDirectory("/build/schema", schema).
		WithNewFile("/build/go.work", goWork).
		WithEnvVariable("GOWORK", "/build/go.work").
		WithEnvVariable("GOTOOLCHAIN", "local").
		WithEnvVariable("GOOS", "tamago").
		WithEnvVariable("GOARCH", "arm64").
		WithEnvVariable("GOOSPKG", "github.com/usbarmory/tamago").
		WithEnvVariable("PATH", tamagoGoBin+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin").
		WithWorkdir("/build/cairn")
}

// elf cross-compiles the AST2700 DC-SCM EVB ELF into /out/cairn.elf.
func (m *Cairn) elf(src, tamago, core, scree, natsServer, aspeedGo, lneto, schema *dagger.Directory) *dagger.Container {
	return m.base(src, tamago, core, scree, natsServer, aspeedGo, lneto, schema).
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
	// +optional
	tamago *dagger.Directory,
	// +optional
	core *dagger.Directory,
	// +optional
	scree *dagger.Directory,
	// +optional
	natsServer *dagger.Directory,
	// +optional
	aspeedGo *dagger.Directory,
	// +optional
	lneto *dagger.Directory,
	// +optional
	schema *dagger.Directory,
) *dagger.File {
	return m.elf(src, tamago, core, scree, natsServer, aspeedGo, lneto, schema).File("/out/cairn.elf")
}

// Test runs the host unit tests (Linux userspace, user_linux build tag).
func (m *Cairn) Test(
	ctx context.Context,
	// +defaultPath="."
	src *dagger.Directory,
	// +optional
	tamago *dagger.Directory,
	// +optional
	core *dagger.Directory,
	// +optional
	scree *dagger.Directory,
	// +optional
	natsServer *dagger.Directory,
	// +optional
	aspeedGo *dagger.Directory,
	// +optional
	lneto *dagger.Directory,
	// +optional
	schema *dagger.Directory,
) (string, error) {
	return m.base(src, tamago, core, scree, natsServer, aspeedGo, lneto, schema).
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
	src, tamago, core, scree, natsServer, aspeedGo, lneto, schema, bmcPb *dagger.Directory,
) *dagger.Container {
	return m.base(src, tamago, core, scree, natsServer, aspeedGo, lneto, schema).
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

// bootMCUFirmwareELF builds the BootMCU firmware ELF from the mcu-runtime
// Rust sources in the StageX Rust image (pinned/overridable via rustImage).
// StageX ships a rustup-less toolchain with no prebuilt riscv32 std, so core is
// built via -Zbuild-std (RUSTC_BOOTSTRAP unlocks it on the pinned stable
// toolchain) — the same approach mcu-runtime's own CI uses.
func (m *Cairn) bootMCUFirmwareELF(
	rustImage string,
	mcuRuntime, aspeedRs, aspeedData *dagger.Directory,
) *dagger.File {
	cargoCache := dag.CacheVolume("cairn-cargo-registry")
	elf := "/build/mcu-runtime/app-rot/target/" +
		bootmcuTarget + "/release/" + bootmcuBin
	return dag.Container().
		From(rustImage).
		WithEnvVariable("CARGO_HOME", "/usr/local/cargo").
		WithEnvVariable("RUSTC_BOOTSTRAP", "1").
		WithMountedCache("/usr/local/cargo/registry", cargoCache).
		WithDirectory("/build/mcu-runtime", mcuRuntime).
		WithDirectory("/build/aspeed-rs", aspeedRs).
		WithDirectory("/build/aspeed-data", aspeedData).
		WithWorkdir("/build/mcu-runtime/app-rot").
		WithEnvVariable("RUSTFLAGS", bootmcuFlags).
		WithExec([]string{"cargo", "build",
			"--target", bootmcuTarget,
			"--release",
			"--bin", bootmcuBin,
			"--no-default-features",
			"--features", "ast2700-bootmcu",
			"-Z", "build-std=core",
		}).
		File(elf)
}

// bootMCU copies the separately-built BootMCU firmware ELF into the image
// container and objcopies it to the raw FMC binary at
// /out/rot_ast2700_bootmcu.fmc.bin. llvm-objcopy comes from the pallet-cgo base.
func (m *Cairn) bootMCU(ctr *dagger.Container, firmware *dagger.File) *dagger.Container {
	return ctr.
		WithFile("/out/rot_ast2700_bootmcu.elf", firmware).
		WithExec([]string{"llvm-objcopy", "-O", "binary",
			"/out/rot_ast2700_bootmcu.elf",
			"/out/rot_ast2700_bootmcu.fmc.bin"}).
		WithWorkdir("/build/cairn")
}

// Image builds the cairn CA35 payload (TamaGo) and stitches it, the BootMCU
// firmware, and the bmc-pb prebuilts (Caliptra, DDR training, DP FW) into a
// complete AST2700-A1 SPI flash image (ASTH secure-boot header) using imgtools.
//
// The BootMCU firmware is either supplied prebuilt via bootMcuFmc (skips the
// Rust build — useful where the StageX Rust image is unavailable) or built from
// the mcu-runtime Rust sources (requires mcuRuntime/aspeedRs/
// aspeedData and a musl-compatible StageX rustImage; core is built via
// -Zbuild-std, so no prebuilt riscv32 std is needed).
//
// A1 ONLY. The A2 image is NOT a drop-in re-stitch: A2 replaces the ASTH header
// with a Caliptra "FLSH" container carrying a signed ATMN SoC auth-manifest, and
// the BootMCU FMC must be linked at GSRAM base 0x14B80000 (A1 links at
// 0x14B80A00 and faults the instant the A2 ROM enters it). The manifest is
// signed by the vendor cptra_imgtool over the exact FMC+payload bytes, which is
// not reproducible in-container, so A2 is produced by tools/a2/gen-a2-image.sh
// (which builds the A2 FMC, generates+signs the manifest and stitches FLSH).
//
// silicon selects the bmc-pb subdirectory; buildTags overrides the payload build
// tags for board variants.
func (m *Cairn) Image(
	ctx context.Context,
	// +defaultPath="."
	src *dagger.Directory,
	// bmcPb holds the non-redistributable vendor prebuilts (Caliptra FW, DDR
	// training, DP FW); it must be supplied — there is no public default.
	bmcPb *dagger.Directory,
	// +optional
	tamago *dagger.Directory,
	// +optional
	core *dagger.Directory,
	// +optional
	scree *dagger.Directory,
	// +optional
	natsServer *dagger.Directory,
	// +optional
	aspeedGo *dagger.Directory,
	// +optional
	lneto *dagger.Directory,
	// +optional
	schema *dagger.Directory,
	// +optional
	bootMcuFmc *dagger.File,
	// +optional
	mcuRuntime *dagger.Directory,
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
	// +default="cairn_ast2700_a1.bin"
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
	if silicon == "a2" {
		return nil, fmt.Errorf("image: --silicon a2 is not built here — the A2 FLSH image needs a vendor-signed ATMN SoC manifest (cptra_imgtool, not reproducible in-container) and the BootMCU FMC linked for A2 (0x14B80000). Use tools/a2/gen-a2-image.sh, which builds the A2 FMC, generates and signs the manifest, and stitches the FLSH container")
	}
	if silicon != "a1" {
		return nil, fmt.Errorf("image: unsupported silicon %q (want a1; for a2 use tools/a2/gen-a2-image.sh)", silicon)
	}
	if imageSize == "" {
		imageSize = "32M"
	}
	if outputName == "" {
		outputName = "cairn_ast2700_a1.bin"
	}
	if buildTags == "" {
		buildTags = defaultBuildTags
	}
	pb := "/build/bmc-pb/ast2700" + silicon

	ctr := m.imageContainer(src, tamago, core, scree, natsServer, aspeedGo, lneto, schema, bmcPb)

	// BootMCU firmware: use the prebuilt FMC when provided, else build from the
	// Rust sources (resolved from public GitHub when not supplied).
	if bootMcuFmc != nil {
		ctr = ctr.WithFile("/out/rot_ast2700_bootmcu.fmc.bin", bootMcuFmc)
	} else {
		firmware := m.bootMCUFirmwareELF(rustImage,
			orRepo(mcuRuntime, "mcu-runtime", branchMain),
			orRepo(aspeedRs, "aspeed-rs", branchMain),
			orRepo(aspeedData, "aspeed-data", branchMain))
		ctr = m.bootMCU(ctr, firmware)
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
	// +optional
	tamago *dagger.Directory,
	// +optional
	core *dagger.Directory,
	// +optional
	scree *dagger.Directory,
	// +optional
	natsServer *dagger.Directory,
	// +optional
	aspeedGo *dagger.Directory,
	// +optional
	lneto *dagger.Directory,
	// +optional
	schema *dagger.Directory,
) (string, error) {
	if _, err := m.elf(src, tamago, core, scree, natsServer, aspeedGo, lneto, schema).Sync(ctx); err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	if _, err := m.Test(ctx, src, tamago, core, scree, natsServer, aspeedGo, lneto, schema); err != nil {
		return "", fmt.Errorf("test: %w", err)
	}
	return "build: ok\ntest: ok", nil
}
