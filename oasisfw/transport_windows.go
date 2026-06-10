//go:build windows

package oasisfw

// Windows HID transport for the Oasis wheel: pure Go over SetupAPI (enumeration)
// + hid.dll (VID/PID) + overlapped ReadFile/WriteFile (the interrupt endpoints) via
// syscall.LazyDLL — no cgo. Vendor HID devices are user-accessible on Windows.

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// GUID_DEVINTERFACE_HID
var hidGUID = guid{0x4D1E55B2, 0xF16F, 0x11CF, [8]byte{0x88, 0xCB, 0x00, 0x11, 0x11, 0x00, 0x00, 0x30}}

const (
	digcfPresent         = 0x02
	digcfDeviceInterface = 0x10
	fileFlagOverlapped   = 0x40000000
	waitObject0          = 0x0
)

var (
	modSetupapi = syscall.NewLazyDLL("setupapi.dll")
	modHid      = syscall.NewLazyDLL("hid.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetClassDevs       = modSetupapi.NewProc("SetupDiGetClassDevsW")
	procEnumInterfaces     = modSetupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procGetInterfaceDetail = modSetupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procDestroyDeviceList  = modSetupapi.NewProc("SetupDiDestroyDeviceInfoList")

	procGetAttributes = modHid.NewProc("HidD_GetAttributes")

	// Std syscall lacks these on Windows; bind them from kernel32 to stay dep-free.
	procCreateEventW        = modKernel32.NewProc("CreateEventW")
	procResetEvent          = modKernel32.NewProc("ResetEvent")
	procGetOverlappedResult = modKernel32.NewProc("GetOverlappedResult")
)

const errIOPending = syscall.Errno(997) // ERROR_IO_PENDING

// createEvent makes a manual-reset, initially-unsignaled event.
func createEvent() (syscall.Handle, error) {
	r, _, err := procCreateEventW.Call(0, 1, 0, 0)
	if r == 0 {
		return 0, err
	}
	return syscall.Handle(r), nil
}

func resetEvent(h syscall.Handle) { procResetEvent.Call(uintptr(h)) }

func getOverlappedResult(h syscall.Handle, ov *syscall.Overlapped, done *uint32, wait bool) error {
	w := uintptr(0)
	if wait {
		w = 1
	}
	r, _, err := procGetOverlappedResult.Call(uintptr(h),
		uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(done)), w)
	if r == 0 {
		return err
	}
	return nil
}

type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGuid guid
	flags              uint32
	reserved           uintptr
}

type hiddAttributes struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
}

type winDev struct {
	path     string
	vid, pid uint16
	loc      uint32 // FNV hash of the device path (stable per device)
}

func openHandle(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, fileFlagOverlapped, 0)
}

// detailPath runs the two-call SetupDiGetDeviceInterfaceDetail pattern and
// returns the device interface path.
func detailPath(h uintptr, ida *spDeviceInterfaceData) string {
	var required uint32
	procGetInterfaceDetail.Call(h, uintptr(unsafe.Pointer(ida)), 0, 0, uintptr(unsafe.Pointer(&required)), 0)
	if required < 6 {
		return ""
	}
	buf := make([]byte, required)
	cb := uint32(8) // cbSize of SP_DEVICE_INTERFACE_DETAIL_DATA_W: 8 on 64-bit, 6 on 32-bit
	if unsafe.Sizeof(uintptr(0)) == 4 {
		cb = 6
	}
	*(*uint32)(unsafe.Pointer(&buf[0])) = cb
	r, _, _ := procGetInterfaceDetail.Call(h, uintptr(unsafe.Pointer(ida)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(required), 0, 0)
	if r == 0 {
		return ""
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[4])), (required-4)/2)
	return syscall.UTF16ToString(u16)
}

func vidPID(path string) (vid, pid uint16, ok bool) {
	h, err := openHandle(path)
	if err != nil {
		return 0, 0, false
	}
	defer syscall.CloseHandle(h)
	var a hiddAttributes
	a.Size = uint32(unsafe.Sizeof(a))
	if r, _, _ := procGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&a))); r == 0 {
		return 0, 0, false
	}
	return a.VendorID, a.ProductID, true
}

func enumerateWindows() ([]winDev, error) {
	h, _, _ := procGetClassDevs.Call(uintptr(unsafe.Pointer(&hidGUID)), 0, 0, digcfPresent|digcfDeviceInterface)
	if h == ^uintptr(0) { // INVALID_HANDLE_VALUE
		return nil, errors.New("SetupDiGetClassDevs failed")
	}
	defer procDestroyDeviceList.Call(h)

	var out []winDev
	var ida spDeviceInterfaceData
	ida.cbSize = uint32(unsafe.Sizeof(ida))
	for i := 0; ; i++ {
		r, _, _ := procEnumInterfaces.Call(h, 0, uintptr(unsafe.Pointer(&hidGUID)),
			uintptr(i), uintptr(unsafe.Pointer(&ida)))
		if r == 0 {
			break // ERROR_NO_MORE_ITEMS
		}
		path := detailPath(h, &ida)
		if path == "" {
			continue
		}
		vid, pid, ok := vidPID(path)
		if !ok || vid != VID || pid != PID {
			continue
		}
		out = append(out, winDev{path: path, vid: vid, pid: pid, loc: hashPath(path)})
	}
	return out, nil
}

func hashPath(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

type windowsTransport struct {
	h      syscall.Handle
	rEvent syscall.Handle
	wEvent syscall.Handle
}

func openWindows(d winDev) (Transport, DeviceInfo, error) {
	h, err := openHandle(d.path)
	if err != nil {
		return nil, DeviceInfo{}, fmt.Errorf("open %s: %w", d.path, err)
	}
	re, err := createEvent()
	if err != nil {
		syscall.CloseHandle(h)
		return nil, DeviceInfo{}, err
	}
	we, err := createEvent()
	if err != nil {
		syscall.CloseHandle(re)
		syscall.CloseHandle(h)
		return nil, DeviceInfo{}, err
	}
	info := DeviceInfo{VID: d.vid, PID: d.pid, LocationID: d.loc}
	return &windowsTransport{h: h, rEvent: re, wEvent: we}, info, nil
}

// Enumerate lists all attached Oasis HID devices.
func Enumerate() ([]DeviceInfo, error) {
	devs, err := enumerateWindows()
	if err != nil {
		return nil, err
	}
	out := make([]DeviceInfo, 0, len(devs))
	for _, d := range devs {
		out = append(out, DeviceInfo{VID: d.vid, PID: d.pid, LocationID: d.loc})
	}
	return out, nil
}

func openFirst() (Transport, DeviceInfo, error) {
	devs, err := enumerateWindows()
	if err != nil {
		return nil, DeviceInfo{}, err
	}
	if len(devs) == 0 {
		return nil, DeviceInfo{}, errors.New("no Oasis filter wheel found")
	}
	return openWindows(devs[0])
}

// OpenLocation opens the Oasis wheel whose path hashes to loc (from Enumerate).
func OpenLocation(loc uint32) (Transport, DeviceInfo, error) {
	devs, err := enumerateWindows()
	if err != nil {
		return nil, DeviceInfo{}, err
	}
	for _, d := range devs {
		if d.loc == loc {
			return openWindows(d)
		}
	}
	return nil, DeviceInfo{}, fmt.Errorf("no Oasis wheel at location 0x%08x", loc)
}

func (t *windowsTransport) Write(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, errors.New("empty buffer")
	}
	resetEvent(t.wEvent)
	ov := &syscall.Overlapped{HEvent: t.wEvent}
	var done uint32
	err := syscall.WriteFile(t.h, buf, &done, ov)
	if err == errIOPending {
		if _, e := syscall.WaitForSingleObject(t.wEvent, syscall.INFINITE); e != nil {
			return 0, e
		}
		err = getOverlappedResult(t.h, ov, &done, false)
	}
	if err != nil {
		return 0, fmt.Errorf("WriteFile: %w", err)
	}
	return int(done), nil
}

// Read waits up to timeoutMS for one interrupt IN report. timeoutMS <= 0 returns
// immediately with whatever is queued (0 bytes if none) — used to drain stale input.
func (t *windowsTransport) Read(buf []byte, timeoutMS int) (int, error) {
	if len(buf) == 0 {
		return 0, errors.New("empty buffer")
	}
	resetEvent(t.rEvent)
	ov := &syscall.Overlapped{HEvent: t.rEvent}
	var done uint32
	err := syscall.ReadFile(t.h, buf, &done, ov)
	if err == errIOPending {
		wait := uint32(timeoutMS)
		if timeoutMS <= 0 {
			wait = 0
		}
		ev, e := syscall.WaitForSingleObject(t.rEvent, wait)
		if e != nil {
			return 0, e
		}
		if ev != waitObject0 { // timed out: cancel the pending read
			syscall.CancelIo(t.h)
			getOverlappedResult(t.h, ov, &done, true)
			return 0, nil
		}
		err = getOverlappedResult(t.h, ov, &done, false)
	}
	if err != nil {
		return 0, fmt.Errorf("ReadFile: %w", err)
	}
	return int(done), nil
}

func (t *windowsTransport) Close() error {
	if t.rEvent != 0 {
		syscall.CloseHandle(t.rEvent)
	}
	if t.wEvent != 0 {
		syscall.CloseHandle(t.wEvent)
	}
	return syscall.CloseHandle(t.h)
}
