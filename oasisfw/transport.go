// Package oasisfw controls Astroasis Oasis filter wheels through HID interrupt reports.
package oasisfw

// Astroasis USB IDs for the Oasis filter wheel.
const (
	VID uint16 = 0x338F
	PID uint16 = 0x0FE0
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
	Serial     string // USB descriptor serial (the device also carries its own SN over the protocol)
	Product    string
	LocationID uint32 // stable USB port path / hidraw index; the handle for OpenLocation
}
