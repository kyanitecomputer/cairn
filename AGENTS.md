# Agent guide — cairn

Context for LLM agents working in this repository. Keep it accurate and generic;
put transient decisions and scratch notes in local (git-ignored) files, not here.

## What this is

Cairn is the Kyanite baseboard management controller (BMC) runtime: a bare-metal
BMC OS for the ASPEED AST2700 (arm64 Cortex-A35) built on
[TamaGo](https://github.com/usbarmory/tamago) — pure Go, no OS, no libc, no CGo.
It shares the `src.kyanite.computer/core` microkernel with `vein`.

## Layout

- `target/<platform>/` — platform-specific code and the entry point (`main.go`).
  The AST2700 DC-SCM target is `target/ast2700-dcscm-evb`.
- `pkg/` — hardware-agnostic, shareable packages (interfaces + value types).
- `.dagger/` — Dagger CI/build module.

No layer imports one above it. A new platform adds a sibling `target/<platform>/`
implementing the `pkg/bmcdev` interface and reuses `pkg/` and `core/` unchanged.

## Build & test

Dagger is the only build interface; it fetches the TamaGo fork, the shared
Kyanite modules and the tamago-go compiler release from public GitHub, so builds
need no local checkouts. Override any source with `--<dir>` for local work.

```sh
dagger call build export --path ./bin   # TamaGo ELF -> bin/cairn.elf
dagger call test                        # host unit tests (user_linux tag)
dagger call ci                          # build + test (what CI runs)
```

Compiles with `GOOS=tamago GOARCH=arm64`. The A1 SPI image is
`dagger call image --bmc-pb <prebuilts> --silicon a1`; the A2 image (vendor-signed
FLSH container, not reproducible in-container) is `tools/a2/gen-a2-image.sh`.

## Conventions

- Configuration is default-struct + normalize + inject (no functional options).
- Everything runs supervised under `core/operator`.
- Follow the org-wide [CONTRIBUTING guide](https://github.com/kyanitecomputer/.github/blob/main/CONTRIBUTING.md):
  Conventional Commits, DCO sign-off (`git commit -s`), SPDX headers, dual
  Apache-2.0 OR MIT licensing.

See `README.md` for details.
