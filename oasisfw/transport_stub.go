//go:build !darwin && !linux && !windows

package oasisfw

import "errors"

// On platforms without a HID backend, these stubs let the package compile and the
// fake-Transport unit tests run with no cgo; the real open/enumerate paths return
// this error until a platform backend exists.
var errNoTransport = errors.New("oasisfw: no HID transport built for this platform (darwin, linux, windows only)")

func openFirst() (Transport, DeviceInfo, error)          { return nil, DeviceInfo{}, errNoTransport }
func OpenLocation(uint32) (Transport, DeviceInfo, error) { return nil, DeviceInfo{}, errNoTransport }
func Enumerate() ([]DeviceInfo, error)                   { return nil, errNoTransport }
