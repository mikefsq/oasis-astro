// Package oasisfoc is a pure-Go driver for the Astroasis Oasis focuser, talking the
// device's USB-HID protocol directly — no vendor SDK in the process.
//
// Like the Oasis filter wheel (the oasisfw package), the focuser is driven over HID
// *interrupt* endpoints (hid_write / hid_read_timeout), not feature reports.

package oasisfoc

// Astroasis USB IDs for the Oasis focuser
const (
	VID uint16 = 0x338F
	PID uint16 = 0xA0F0
)

// Transport is the minimal HID interrupt channel the Oasis focuser needs. A backend
// (IOKit, hidraw, hid.dll) provides it; the device logic is transport-agnostic.
//
// Write sends one interrupt OUT report (buf[0] is the HID report ID, 0x00). Read
// reads one interrupt IN report, waiting up to timeoutMS (<=0 = poll, used to drain
// stale input before a command). Both return the number of bytes transferred.
type Transport interface {
	Write(buf []byte) (int, error)
	Read(buf []byte, timeoutMS int) (int, error)
	Close() error
}

// DeviceInfo describes an enumerated/opened HID device.
type DeviceInfo struct {
	VID, PID   uint16
	Serial     string
	Product    string
	LocationID uint32
}
