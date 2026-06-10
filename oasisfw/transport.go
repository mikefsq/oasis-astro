// Package oasisfw is a pure-Go driver for the Astroasis Oasis filter wheel,
// talking the device's USB-HID protocol directly — no vendor SDK in the process.
//
// Unlike the ZWO EFW (which uses HID *feature* reports), the Oasis wheel is driven
// over HID *interrupt* endpoints: commands go out via an interrupt OUT report and
// replies come back on interrupt IN.
package oasisfw

// Astroasis USB IDs for the Oasis filter wheel.
const (
	VID uint16 = 0x338F
	PID uint16 = 0x0FE0
)

// Transport is the minimal HID interrupt channel the Oasis wheel needs. A backend
// (IOKit, hidraw, hid.dll) provides it; the device logic above is transport-agnostic.
//
// Write sends one interrupt OUT report; buf[0] is the HID report ID (0x00 for this
// device). Read reads one interrupt IN report, waiting up to timeoutMS (<=0 = poll,
// returning 0 bytes if nothing is queued — used to drain stale input before a
// command). Both return the number of bytes transferred.
type Transport interface {
	Write(buf []byte) (int, error)
	Read(buf []byte, timeoutMS int) (int, error)
	Close() error
}

// DeviceInfo describes an enumerated/opened HID device.
type DeviceInfo struct {
	VID, PID   uint16
	Serial     string // USB descriptor serial (the device also carries its own SN over the protocol)
	Product    string
	LocationID uint32 // stable USB port path / hidraw index; the handle for OpenLocation
}
