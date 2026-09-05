// Package oasisfoc controls Astroasis Oasis focusers through HID interrupt reports.

package oasisfoc

// Astroasis USB IDs for the Oasis focuser
const (
	VID uint16 = 0x338F
	PID uint16 = 0xA0F0
)

// Transport reads and writes HID interrupt reports. Write includes report ID 0.
// Read polls when timeoutMS <= 0 and returns zero bytes when no report is queued.
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
