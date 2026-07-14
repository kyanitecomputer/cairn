// Package config implements typed configuration management for a Kyanite BMC,
// built on top of core/cfgstore (Scree-backed KV).
//
// # Storage
//
// All values are stored as UTF-8 strings in the KV store. Structured types (IP
// addresses, MAC) are encoded as simple colon/slash-separated strings — no
// JSON, to keep code size minimal.
//
// Keys use a dotted namespace:
//
//	sys.hostname          string
//	sys.mgmt_ip           CIDR string, e.g. "192.168.0.2/24"
//	sys.mgmt_mac          hex string, e.g. "02:00:00:00:00:02"
//
// # Defaults
//
// If a key is absent (or corrupt) in the store, the accessor returns the
// corresponding field from the [Defaults] the platform target supplied to
// [New]. A freshly erased flash partition therefore yields a valid, working
// configuration. The package is platform-neutral: the concrete factory identity
// (hostname, mgmt IP, MAC) lives with the hardware target, not here.
//
// This mirrors vein's pkg/config: the same default-struct + normalize + inject
// model, so both device runtimes share one configuration idiom.
package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"src.kyanite.computer/core/cfgstore"
)

// Defaults holds the factory-default system configuration a platform target
// supplies to [New]. These are the values returned when the corresponding key
// is absent in the store. Unset fields are filled from [DefaultDefaults] by
// [Normalize], which New calls automatically.
type Defaults struct {
	// Hostname is the factory-default BMC hostname.
	Hostname string
	// MgmtIP is the factory-default management IP prefix (address + length).
	MgmtIP netip.Prefix
	// MgmtMAC is the factory-default management-port MAC address.
	MgmtMAC [6]byte
}

// DefaultDefaults returns the generic, platform-neutral factory defaults used
// to fill any field a target leaves unset. Targets override the fields that
// identify their hardware.
func DefaultDefaults() Defaults {
	return Defaults{
		Hostname: "bmc",
		MgmtIP:   netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, 0, 2}), 24),
		MgmtMAC:  [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02},
	}
}

// Normalize fills unset fields from [DefaultDefaults]. A zero MgmtMAC and an
// invalid MgmtIP are treated as unset.
func (d *Defaults) Normalize() {
	def := DefaultDefaults()
	if d.Hostname == "" {
		d.Hostname = def.Hostname
	}
	if !d.MgmtIP.IsValid() {
		d.MgmtIP = def.MgmtIP
	}
	if d.MgmtMAC == ([6]byte{}) {
		d.MgmtMAC = def.MgmtMAC
	}
}

// Manager wraps a cfgstore.Store to provide typed config access, returning
// platform-supplied defaults for absent keys.
type Manager struct {
	store *cfgstore.Store
	def   Defaults
}

// New creates a Config Manager backed by the given cfgstore Store, using def as
// the factory defaults for absent keys. Unset fields in def are filled from
// [DefaultDefaults].
func New(store *cfgstore.Store, def Defaults) *Manager {
	def.Normalize()
	return &Manager{store: store, def: def}
}

// --- System config ----------------------------------------------------------

// Hostname returns the configured hostname.
func (m *Manager) Hostname() string {
	return m.getString("sys.hostname", m.def.Hostname)
}

// SetHostname sets the BMC hostname.
func (m *Manager) SetHostname(name string) error {
	return m.setString("sys.hostname", name)
}

// MgmtIP returns the management IP prefix (e.g. "192.168.0.2/24"). Returns the
// default if not configured.
func (m *Manager) MgmtIP() netip.Prefix {
	s := m.getString("sys.mgmt_ip", m.def.MgmtIP.String())
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return m.def.MgmtIP
	}
	return p
}

// SetMgmtIP stores the management IP prefix.
func (m *Manager) SetMgmtIP(prefix netip.Prefix) error {
	return m.setString("sys.mgmt_ip", prefix.String())
}

// MgmtMAC returns the management-port MAC address.
func (m *Manager) MgmtMAC() [6]byte {
	s := m.getString("sys.mgmt_mac", macString(m.def.MgmtMAC))
	mac, err := parseMACString(s)
	if err != nil {
		return m.def.MgmtMAC
	}
	return mac
}

// SetMgmtMAC stores the management MAC address.
func (m *Manager) SetMgmtMAC(mac [6]byte) error {
	return m.setString("sys.mgmt_mac", macString(mac))
}

// SystemSummary returns a printable summary of the system configuration.
func (m *Manager) SystemSummary() string {
	return fmt.Sprintf("hostname=%s mgmt_ip=%s mac=%s",
		m.Hostname(), m.MgmtIP(), macString(m.MgmtMAC()))
}

// Store returns the underlying cfgstore.Store, for packages that persist their
// own keys.
func (m *Manager) Store() *cfgstore.Store {
	return m.store
}

// --- low-level helpers ------------------------------------------------------

func (m *Manager) getString(key, def string) string {
	v, err := m.store.Get(key)
	if err != nil {
		return def
	}
	return string(v)
}

func (m *Manager) setString(key, val string) error {
	return m.store.Set(key, []byte(val))
}

func macString(mac [6]byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
}

func parseMACString(s string) ([6]byte, error) {
	var mac [6]byte
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return mac, fmt.Errorf("config: invalid MAC %q", s)
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return mac, err
		}
		mac[i] = byte(v)
	}
	return mac, nil
}
