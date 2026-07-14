# Cairn

Cairn is the Kyanite baseboard management controller (BMC) software stack. It is
a bare-metal BMC OS built with the [TamaGo](https://github.com/usbarmory/tamago)
framework: pure Go, no OS, no libc, no CGo. It shares the microkernel substrate
(`src.kyanite.computer/core`) with [vein](../vein), the Kyanite switch OS.

This is early scaffolding: the first target is a structural stub that establishes
the layering and patterns. It performs no hardware management yet — sensor,
power and inventory drivers land during hardware enablement.

## Hardware

| Component | Detail |
|-----------|--------|
| SoC  | ASPEED AST2700 (dual Cortex-A35, ARMv8-A / arm64) |
| RAM  | 1 GB DRAM, CA35 view at `0x400000000` |
| Board| AST2700 DC-SCM EVB (AST2750-A1 DDR4) |

## Layout

Three layers, strictly by sharing scope (no layer imports one above it) — the
same split as vein:

```
target/ast2700-dcscm-evb/   AST2700 DC-SCM PLATFORM code. Everything hardware-specific.
  main.go                     entry point; assembles the microkernel via core/operator
  board/                      platform identity; imports the upstream TamaGo board
                              package for CA35 bring-up (no in-tree hardware code yet)
  chassis/                    baseboard-management driver (implements pkg/bmcdev.Chassis; stub)

pkg/                        SHAREABLE across cairn targets; hardware-agnostic (deps on interfaces).
  bmcdev/                     baseboard-management interface + shared value types
  config/                     typed configuration layer (Defaults + Normalize pattern)

.dagger/                    Dagger CI/build module
```

The microkernel substrate (service supervision, operator, telemetry, config
store) is the shared `src.kyanite.computer/core` module, consumed by both cairn
and vein. `target/ast2700-dcscm-evb/main.go` assembles it declaratively via
`core/operator` (a plain `operator.Config` — no functional options — with a
caller-owned service slice). A new platform adds a sibling `target/<platform>/`
implementing the `pkg/bmcdev` interface and reuses everything in `pkg/` and
`core/` unchanged.

## Patterns

Cairn follows the same embedded-friendly conventions as vein:

- **Configuration** is a default struct + normalize + inject, not functional
  options: `config.DefaultDefaults()` / `operator.DefaultConfig()`, overridden by
  value and normalized, with caller-owned storage. This keeps memory ownership
  explicit and avoids closure/slice allocation on memory-constrained targets.
- **Platform identity lives with the target.** `pkg/` holds no hardware detail;
  the factory hostname/IP/MAC and the concrete chassis driver are supplied by
  `target/ast2700-dcscm-evb`.
- **Everything is supervised.** Services implement `core/service.Service` and run
  under `core/operator`, which recovers panics and restarts per policy.

## Build

The AST2700 arm64 SoC/board support lives in a TamaGo fork that is not yet
upstream. Until it lands, `go.work` redirects `github.com/usbarmory/tamago` to
that fork (`../tamago`).

```bash
make build       # AST2700 DC-SCM EVB ELF → bin/cairn.elf
make test        # host unit tests (user_linux tag)
```

Requires the TamaGo compiler (`TAMAGO=/path/to/tamago-go/bin/go`) and
`llvm-readelf`.

## Module

```
src.kyanite.computer/cairn
```

No CGo, no external C libraries. Compiles with `GOOS=tamago GOARCH=arm64`.
