//go:build linux

package oasisfw

// Linux HID transport for the Oasis wheel: pure Go over hidraw (/dev/hidrawN).
// The Oasis wheel uses interrupt endpoints, so commands/replies are ordinary
// write()/read() on the hidraw fd (not feature-report ioctls). Requires a udev
// rule so the service user can open the device, e.g.:
//
//	KERNEL=="hidraw*", ATTRS{idVendor}=="338f", MODE="0660", TAG+="uaccess"
//

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const hidrawSysfs = "/sys/class/hidraw"

type linuxDev struct {
	path     string // /dev/hidrawN
	vid, pid uint16
	loc      uint32 // hidraw number (session-stable)
}

// scanHidraw lists hidraw devices and their VID/PID from sysfs.
func scanHidraw() []linuxDev {
	entries, err := os.ReadDir(hidrawSysfs)
	if err != nil {
		return nil
	}
	var out []linuxDev
	for _, e := range entries {
		name := e.Name() // "hidrawN"
		vid, pid, ok := parseUevent(filepath.Join(hidrawSysfs, name, "device", "uevent"))
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "hidraw"))
		out = append(out, linuxDev{path: "/dev/" + name, vid: vid, pid: pid, loc: uint32(n)})
	}
	return out
}

// parseUevent reads "HID_ID=0003:0000338F:00000FE0" (bus:vid:pid) from a hidraw
// device's uevent.
func parseUevent(path string) (vid, pid uint16, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "HID_ID=") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(line, "HID_ID="), ":")
		if len(parts) != 3 {
			return 0, 0, false
		}
		v, e1 := strconv.ParseUint(parts[1], 16, 32)
		p, e2 := strconv.ParseUint(parts[2], 16, 32)
		if e1 != nil || e2 != nil {
			return 0, 0, false
		}
		return uint16(v), uint16(p), true
	}
	return 0, 0, false
}

func matchOasis(d linuxDev) bool { return d.vid == VID && d.pid == PID }

type linuxTransport struct{ fd int }

func openLinux(d linuxDev) (Transport, DeviceInfo, error) {
	// O_NONBLOCK + raw syscall.Read/Write keeps Read's timeout portable across
	// amd64/arm64 (no poll/ppoll arch split) and avoids the Go runtime poller.
	fd, err := syscall.Open(d.path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, DeviceInfo{}, fmt.Errorf("open %s: %w (need a udev rule? "+
			`KERNEL=="hidraw*", ATTRS{idVendor}=="338f", MODE="0660", TAG+="uaccess")`, d.path, err)
	}
	info := DeviceInfo{VID: d.vid, PID: d.pid, LocationID: d.loc}
	return &linuxTransport{fd: fd}, info, nil
}

// Enumerate lists all attached Oasis hidraw devices.
func Enumerate() ([]DeviceInfo, error) {
	var out []DeviceInfo
	for _, d := range scanHidraw() {
		if matchOasis(d) {
			out = append(out, DeviceInfo{VID: d.vid, PID: d.pid, LocationID: d.loc})
		}
	}
	return out, nil
}

func openFirst() (Transport, DeviceInfo, error) {
	for _, d := range scanHidraw() {
		if matchOasis(d) {
			return openLinux(d)
		}
	}
	return nil, DeviceInfo{}, errors.New("no Oasis filter wheel found in /sys/class/hidraw")
}

// OpenLocation opens the Oasis wheel at a specific hidraw location (from Enumerate).
func OpenLocation(loc uint32) (Transport, DeviceInfo, error) {
	for _, d := range scanHidraw() {
		if matchOasis(d) && d.loc == loc {
			return openLinux(d)
		}
	}
	return nil, DeviceInfo{}, fmt.Errorf("no Oasis wheel at hidraw location %d", loc)
}

func (t *linuxTransport) Write(buf []byte) (int, error) { return syscall.Write(t.fd, buf) }

// Read waits up to timeoutMS for one interrupt IN report. timeoutMS <= 0 does a
// single non-blocking read (returns 0 bytes if nothing is queued) — used to drain
// stale input. The fd is O_NONBLOCK, so we retry on EAGAIN until the deadline.
func (t *linuxTransport) Read(buf []byte, timeoutMS int) (int, error) {
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	for {
		n, err := syscall.Read(t.fd, buf)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, syscall.EAGAIN) {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return 0, err
		}
		// nothing queued yet (EAGAIN or n==0)
		if timeoutMS <= 0 || !time.Now().Before(deadline) {
			return 0, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (t *linuxTransport) Close() error { return syscall.Close(t.fd) }
