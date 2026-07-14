// Package bmcdev defines the hardware-agnostic baseboard-management contract
// shared by cairn's target-independent services.
//
// The concrete management driver (per hardware platform, e.g.
// target/ast2700-dcscm-evb) drives the vendor GPIO/PMBus/I2C fabric and
// implements [Chassis]; the shared services depend only on this interface and
// the value types here, so they contain no platform detail. This mirrors the
// role vein's pkg/switchdev plays for the switch data plane: cairn manages a
// host baseboard, vein manages a switch fabric, but both invert platform
// specifics behind a small interface so pkg/ and core/ stay reusable.
//
// It is deliberately minimal: this is the seed of the BMC management surface,
// grown as real sensor/power/inventory drivers land during hardware
// enablement.
package bmcdev

// PowerState enumerates the managed host's chassis power condition.
type PowerState uint8

const (
	// PowerUnknown means the driver has not yet determined the host power
	// state (for example before the first read).
	PowerUnknown PowerState = iota
	// PowerOff means the host is powered down.
	PowerOff
	// PowerOn means the host is powered up.
	PowerOn
)

// String returns a short human-readable label for the power state.
func (p PowerState) String() string {
	switch p {
	case PowerOff:
		return "off"
	case PowerOn:
		return "on"
	default:
		return "unknown"
	}
}

// Chassis is the management surface of a managed host baseboard, as used by the
// target-independent services. A platform's concrete driver implements it.
type Chassis interface {
	// Model returns the human-readable board/chassis model identifier.
	Model() string
	// Power reports the current host chassis power state.
	Power() PowerState
	// SetPower requests a host power transition (on powers up, off powers
	// down). It returns an error if the transition cannot be initiated.
	SetPower(on bool) error
}
