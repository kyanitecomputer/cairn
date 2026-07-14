// Package chassis is the AST2700 DC-SCM EVB baseboard-management driver.
//
// It is the platform-layer implementation of pkg/bmcdev.Chassis, the analog of
// vein's target/vega/sw switch driver. This is a structural stub: it satisfies
// the interface with in-memory state and performs no hardware access. The real
// GPIO/PMBus/I2C power sequencing lands during hardware enablement; keeping the
// interface wired now lets the target's main and any shared services be
// assembled and supervised today.
package chassis

import "src.kyanite.computer/cairn/pkg/bmcdev"

// Driver is the stub AST2700 DC-SCM chassis manager. It tracks power state in
// memory only.
type Driver struct {
	model string
	power bmcdev.PowerState
}

// Compile-time assertion that *Driver satisfies the management contract.
var _ bmcdev.Chassis = (*Driver)(nil)

// New returns a stub chassis driver reporting the given model. Power starts in
// the unknown state, as no hardware has been queried.
func New(model string) *Driver {
	return &Driver{model: model, power: bmcdev.PowerUnknown}
}

// Model returns the board/chassis model identifier.
func (d *Driver) Model() string { return d.model }

// Power reports the last known host power state. The stub never observes
// hardware, so this reflects only prior SetPower calls.
func (d *Driver) Power() bmcdev.PowerState { return d.power }

// SetPower records the requested power state. The stub performs no hardware
// sequencing; it always succeeds.
func (d *Driver) SetPower(on bool) error {
	if on {
		d.power = bmcdev.PowerOn
	} else {
		d.power = bmcdev.PowerOff
	}
	return nil
}
