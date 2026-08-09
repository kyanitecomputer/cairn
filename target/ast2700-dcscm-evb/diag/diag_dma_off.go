// DMA-scaling probe disabled. The default flashdiag image omits the DMA probe
// because it writes controller DMA registers and polls for completion; a wrong
// address encoding could stall the engine. Build with an extra `flashdiagdma`
// tag to enable it once the safe register/window probes have passed.

//go:build tamago && !flashdiagdma

package diag

func dmaProbe() {
	p("[Q0.4b] DMA-scaling probe: DISABLED")
	p("        rebuild the payload with -tags ...,flashdiag,flashdiagdma to run it")
}
