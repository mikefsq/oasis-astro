package oasisfw

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// --- Oasis HID framing
//
// The vendor builds a small command descriptor {opcode, payloadLen, payload...},
// prepends a 0x00 report-ID byte, zero-pads to a 65-byte report, and writes it on
// the interrupt OUT endpoint; the reply comes back on interrupt IN.
//
//	command : [0]=0x00 reportID  [1]=opcode  [2]=payloadLen  [3..]=payload (padded to 65)
//	reply   : [0]=opcode echo    [1]=len     [2..]=data
//
// Multi-byte numeric fields are BIG-ENDIAN on the wire.
// The reply data offset is not uniform across commands: status data
// starts at reply[2], the focus/color page tables at reply[3].
//
// The transceiver drains any stale IN reports, writes the command, then reads the
// reply.
const (
	reportLen   = 65   // 1 report-ID byte + 64-byte payload
	reportID    = 0x00 // this device uses report ID 0 (unnumbered); it leads the OUT buffer
	replyWaitMS = 100  // hid_read_timeout

	// A transient HID write failure (USB power-management wake on a Full-Speed device,
	// a hub transaction-translator stall, or a brief IOKit hiccup) is retried before the
	// device is declared lost. Safe to resend: a failed IOHIDDeviceSetReport did not
	// deliver the report, so the device receives the command exactly once.
	writeTries        = 3
	writeRetryBackoff = 8 * time.Millisecond

	// Opcodes (command descriptor byte[0]). Get/set pairs differ by 1.
	opInfoA = 0x02 // version (reply 36B = version fields + build-date string)
	opInfoB = 0x01 // product model (reply 32B NUL-terminated)

	opSerial           = 0x03 // factory serial number (read)
	opGetFriendlyName  = 0x04 // user "friendly" name (read)
	opSetFriendlyName  = 0x05 // user "friendly" name (write)
	opGetBluetoothName = 0x06 // bluetooth name (read)
	opSetBluetoothName = 0x07 // bluetooth name (write)

	opConfig       = 0x30 // device config block (read)
	opSetConfig    = 0x31 // device config block (write)
	opStatus       = 0x32 // status: position + state (read)
	opFactoryReset = 0x33 // restore factory defaults (write)

	opSlotNum        = 0x50 // slot (filter) count (read)
	opGetSlotName    = 0x51 // per-slot name (read); payload [slot]
	opSetSlotName    = 0x52 // per-slot name (write); payload [slot, name...]
	opGetFocusOffset = 0x53 // focus offsets (read); 8-entry int32 BE page table
	opSetFocusOffset = 0x54 // focus offsets (write); 8-entry int32 BE page table
	opGetColor       = 0x55 // per-slot color (read); 8-entry int32 BE page table
	opSetColor       = 0x56 // per-slot color (write); 8-entry int32 BE page table
	opSetPosition    = 0x57 // move to a slot (write); payload [target]
	opCalibrate      = 0x58 // home + re-align (write); descriptor byte[1]=0x01
)

// Status reply layout: temperature and counter are big-endian int32; filterStatus
// and filterPosition are single bytes.
const (
	statusTempOff    = 2 // temperature: int32 big-endian (ntohl)
	statusByteState  = 6 // filterStatus (see state* below)
	statusBytePos    = 7 // filterPosition: 1-based on the wire (0 = unknown/not homed)
	statusCounterOff = 8 // counter: int32 big-endian (ntohl)
)

// filterStatus values
const (
	stateIdle         = 0 // ready / idle
	stateMoving       = 1
	stateCalibrating  = 2
	stateBenchmarking = 3
)

// Oasis is an opened filter wheel.
type Oasis struct {
	t    Transport
	info DeviceInfo

	mu sync.Mutex // serializes a command + its reply per device

	// raw replies to the identify handshake (opInfoA/opInfoB), cached so VersionRaw
	// and ModelRaw can read device identity.
	infoA, infoB []byte
}

// New wraps an already-open Transport as an Oasis handle. Most callers use
// OpenFirst / OpenBySerial; New is for supplying a custom Transport — an alternate
// backend, or a fake for testing the stack without hardware.
func New(t Transport, info DeviceInfo) *Oasis { return &Oasis{t: t, info: info} }

// OpenFirst finds and opens the first attached Oasis filter wheel.
func OpenFirst() (*Oasis, error) {
	t, info, err := openFirst()
	if err != nil {
		return nil, err
	}
	o := New(t, info)
	o.Handshake() // best-effort identify; failures left to caller via VersionRaw
	return o, nil
}

// OpenAt opens the Oasis wheel at a specific USB locationID (from Enumerate / List).
func OpenAt(locationID uint32) (*Oasis, error) {
	t, info, err := OpenLocation(locationID)
	if err != nil {
		return nil, err
	}
	o := New(t, info)
	o.Handshake()
	return o, nil
}

func (o *Oasis) Info() DeviceInfo { return o.info }
func (o *Oasis) Close() error     { return o.t.Close() }

// command runs one transceiver round-trip: drain stale input, write
// [0x00, opcode, len, payload...], then read the reply. It returns the reply
// payload (reply[0]=opcode echo, reply[1]=len, reply[2:]=data). Caller holds mu.
func (o *Oasis) command(opcode byte, payload []byte) ([]byte, error) {
	if len(payload)+2 > reportLen-1 {
		return nil, fmt.Errorf("oasis: payload too long (%d)", len(payload))
	}

	// Drain any stale IN reports
	scratch := make([]byte, reportLen)
	for {
		n, err := o.t.Read(scratch, 0)
		if err != nil || n <= 0 {
			break
		}
	}

	frame := make([]byte, reportLen)
	frame[0] = reportID
	frame[1] = opcode
	frame[2] = byte(len(payload))
	copy(frame[3:], payload)
	var werr error
	for try := 0; try < writeTries; try++ {
		if _, werr = o.t.Write(frame); werr == nil {
			break
		}
		time.Sleep(writeRetryBackoff) // transient stall — brief backoff, then resend
	}
	if werr != nil {
		return nil, fmt.Errorf("oasis: write opcode 0x%02x (after %d tries): %w", opcode, writeTries, werr)
	}

	// Read the reply, SKIPPING any stale report whose echo != our opcode. The interrupt-IN
	// drain above is racy — a reply to a previous command (e.g. a large multi-field read like
	// the 0x55 color page just before a 0x57 move) can land in the queue after the drain and
	// before our reply, so the first report read may be that leftover. Discard mismatches and
	// read the next, bounded so a genuinely silent opcode still fails promptly.
	for attempts := 0; attempts < 8; attempts++ {
		reply := make([]byte, reportLen)
		n, err := o.t.Read(reply, replyWaitMS)
		if err != nil {
			return nil, fmt.Errorf("oasis: read reply to 0x%02x: %w", opcode, err)
		}
		if n == 0 {
			return nil, fmt.Errorf("oasis: no reply to 0x%02x", opcode)
		}
		if reply[0] == opcode {
			return reply[:n], nil
		}
		// stale reply (echo 0x%02x != ours) — discard and read the next report
	}
	return nil, fmt.Errorf("oasis: no matching reply to 0x%02x (only stale reports)", opcode)
}

// Command issues a raw command and returns the raw reply, for
// validation/debugging against real hardware.
func (o *Oasis) Command(opcode byte, payload []byte) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.command(opcode, payload)
}

// status fetches the raw status reply. Caller holds mu.
func (o *Oasis) status() ([]byte, error) { return o.command(opStatus, nil) }

// Status returns the raw status reply, for debugging.
func (o *Oasis) Status() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status()
}

// Position returns the current 0-based slot, or -1 if the wheel is busy
// (moving/calibrating/etc.) or its position is unknown. The wire reports a 1-based
// filterPosition (0 = unknown/not homed); we present 0-based per ASCOM.
func (o *Oasis) Position() (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.status()
	if err != nil {
		return -1, err
	}
	if len(r) <= statusBytePos {
		return -1, errors.New("oasis: short status reply")
	}
	if r[statusByteState] != stateIdle {
		return -1, nil // moving / calibrating / benchmarking
	}
	p := int(r[statusBytePos])
	if p == 0 {
		return -1, nil // unknown / not homed
	}
	return p - 1, nil
}

// State returns the raw filterStatus value (stateIdle/stateMoving/…).
func (o *Oasis) State() (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.status()
	if err != nil {
		return 0, err
	}
	if len(r) <= statusByteState {
		return 0, errors.New("oasis: short status reply")
	}
	return int(r[statusByteState]), nil
}

// TemperatureRaw returns the raw internal-thermistor ADC value.
func (o *Oasis) TemperatureRaw() (int32, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.status()
	if err != nil {
		return 0, err
	}
	if len(r) < statusTempOff+4 {
		return 0, errors.New("oasis: short status reply")
	}
	return int32(binary.BigEndian.Uint32(r[statusTempOff : statusTempOff+4])), nil
}

// Temperature converts the raw internal-thermistor ADC to °C using
// a standard 10k NTC (β=3380, T0=25 °C) curve.
func (o *Oasis) Temperature() (float64, error) {
	raw, err := o.TemperatureRaw()
	if err != nil {
		return 0, err
	}
	const (
		beta    = 3380.0
		bOverT0 = 11.3367
		kelvin  = 273.15
	)
	adc := float64(raw)
	if adc < 1 {
		adc = 1
	} else if adc > 4094 {
		adc = 4094
	}
	return beta/(math.Log((4095.0-adc)/adc)+bOverT0) - kelvin, nil
}

// SetPosition moves the wheel to the given 0-based slot. Returns once the command
// is acknowledged; poll Position for completion (-1 while moving).
func (o *Oasis) SetPosition(slot int) error {
	if slot < 0 || slot > 0xff {
		return fmt.Errorf("oasis: slot %d out of range", slot)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opSetPosition, []byte{byte(slot + 1)}) // wire is 1-based
	return err
}

// Slots returns the wheel's filter-slot count (the first data byte of the
// slot-count reply, reply[2]).
func (o *Oasis) Slots() (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.slotsLocked()
}

// slotsLocked returns the slot count (reply[2]). Caller holds mu.
func (o *Oasis) slotsLocked() (int, error) {
	r, err := o.command(opSlotNum, nil)
	if err != nil {
		return 0, err
	}
	if len(r) < 3 {
		return 0, errors.New("oasis: short slot-count reply")
	}
	return int(r[2]), nil
}

// Calibrate runs the wheel's home + slot-realignment routine. It spins the wheel
// for several seconds; poll Position (-1 while moving) for completion.
func (o *Oasis) Calibrate() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opCalibrate, []byte{0x00})
	return err
}

// SerialRaw returns the raw serial-number reply payload (reply[2:]), for
// debugging.
func (o *Oasis) SerialRaw() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.readRaw(opSerial)
}

// ConfigRaw returns the raw config-block reply payload (reply[2:]), for debugging.
// See Config for the decoded form.
func (o *Oasis) ConfigRaw() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.readRaw(opConfig)
}

// Config mirrors the device's config block (mask = big-endian int32 at +2; the rest
// single bytes at +6..+9). Hardware-confirmed 2026-06-10: a no-op write round-trips
// byte-identical and toggling turbo changes only its byte.
type Config struct {
	Mask        uint32
	Speed       int
	Autorun     int
	BluetoothOn int
	Turbo       int
}

// Config reads and decodes the device config block.
func (o *Oasis) Config() (Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opConfig, nil)
	if err != nil {
		return Config{}, err
	}
	if len(r) < 10 {
		return Config{}, errors.New("oasis: short config reply")
	}
	return Config{
		Mask:        binary.BigEndian.Uint32(r[2:6]),
		Speed:       int(r[6]),
		Autorun:     int(r[7]),
		BluetoothOn: int(r[8]),
		Turbo:       int(r[9]),
	}, nil
}

// SetConfigRaw writes a raw config block (opcode 0x31). Passthrough escape hatch; the
// typed setters below are preferred.
func (o *Oasis) SetConfigRaw(block []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opSetConfig, block)
	return err
}

// patchConfig read-modify-writes the config block at the byte level (read 0x30, patch,
// write 0x31), so unread/reserved bytes are preserved exactly. Field byte offsets in the
// data block (reply[2:]): mask@0:4, speed@4, autorun@5, bluetoothOn@6, turbo@7.
func (o *Oasis) patchConfig(patch func(raw []byte)) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opConfig, nil)
	if err != nil {
		return err
	}
	// Bound the block by the reply's declared length byte (reply[1]) — the read returns a
	// full 64-byte report, but the config block is reply[1] bytes (0x1f=31); SetConfig (0x31)
	// expects exactly that payload length, so trailing report bytes must not be sent.
	n := int(r[1])
	if n < 8 || 2+n > len(r) {
		return errors.New("oasis: short config reply")
	}
	raw := append([]byte(nil), r[2:2+n]...)
	patch(raw)
	_, err = o.command(opSetConfig, raw)
	return err
}

// SetSpeed sets the wheel rotation speed.
func (o *Oasis) SetSpeed(v int) error { return o.patchConfig(func(r []byte) { r[4] = byte(v) }) }

// SetAutorun enables/disables auto-run (rotate to slot 1 on power-up).
func (o *Oasis) SetAutorun(on bool) error { return o.patchConfig(func(r []byte) { r[5] = b2byte(on) }) }

// SetBluetoothOn enables/disables Bluetooth.
func (o *Oasis) SetBluetoothOn(on bool) error {
	return o.patchConfig(func(r []byte) { r[6] = b2byte(on) })
}

// SetTurbo enables/disables turbo (fast) mode.
func (o *Oasis) SetTurbo(on bool) error { return o.patchConfig(func(r []byte) { r[7] = b2byte(on) }) }

func b2byte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// FactoryReset restores the wheel's factory defaults.
func (o *Oasis) FactoryReset() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opFactoryReset, nil)
	return err
}

// --- Identity: handshake + cached version/model ---

// Handshake issues the two identify commands at connect (opInfoA then opInfoB) and
// caches their raw replies, which feed VersionRaw/ModelRaw. It is called
// automatically by OpenFirst/OpenAt; callers using New may invoke it themselves.
func (o *Oasis) Handshake() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	a, err := o.command(opInfoA, nil)
	if err != nil {
		return err
	}
	b, err := o.command(opInfoB, nil)
	if err != nil {
		return err
	}
	o.infoA = append([]byte(nil), a...)
	o.infoB = append([]byte(nil), b...)
	return nil
}

// VersionRaw returns the cached opInfoA handshake reply (the version block).
// Hardware-confirmed: decodes to HW 2.4.0.0, FW 1.7.1.0, built "Apr 23 2026, 17:28:10".
// nil if Handshake hasn't run.
func (o *Oasis) VersionRaw() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.infoA...)
}

// ModelRaw returns the cached opInfoB handshake reply (the product-model string).
// nil if Handshake hasn't run.
func (o *Oasis) ModelRaw() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.infoB...)
}

// cstr reads a NUL-terminated ASCII string from b.
func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Serial returns the factory serial number as a string.
func (o *Oasis) Serial() (string, error) {
	raw, err := o.SerialRaw()
	if err != nil {
		return "", err
	}
	return cstr(raw), nil
}

// Model returns the product model string (the 0x01 handshake reply, e.g. "OasisFilterWheel").
func (o *Oasis) Model() string {
	b := o.ModelRaw()
	if len(b) < 2 {
		return ""
	}
	return cstr(b[2:])
}

// The version reply (opcode 0x02) is three 4-byte version fields — protocol, hardware,
// firmware, each major.minor.patch.build — followed by a NUL-terminated firmware
// build-date string. Hardware-confirmed (HW 2.4.0.0, FW 1.7.1.0, built
// "Apr 23 2026, 17:28:10"); identical layout to the Oasis focuser.
const (
	verProtocol = 0
	verHardware = 4
	verFirmware = 8
	verBuilt    = 12
)

func (o *Oasis) versionField(off int) []byte {
	v := o.VersionRaw() // full reply: [echo, len, data...]
	if len(v) < 2+off+4 {
		return nil
	}
	return v[2+off : 2+off+4]
}

func ver4(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}

// ProtocolVersion / HardwareVersion / FirmwareVersion return "major.minor.patch.build"
// (e.g. "2.4.0.0" / "1.7.1.0").
func (o *Oasis) ProtocolVersion() string { return ver4(o.versionField(verProtocol)) }
func (o *Oasis) HardwareVersion() string { return ver4(o.versionField(verHardware)) }
func (o *Oasis) FirmwareVersion() string { return ver4(o.versionField(verFirmware)) }

// FirmwareBuildDate returns the firmware build timestamp (e.g. "Apr 23 2026, 17:28:10").
func (o *Oasis) FirmwareBuildDate() string {
	v := o.VersionRaw()
	if len(v) < 2+verBuilt {
		return ""
	}
	return cstr(v[2+verBuilt:])
}

// --- Names (friendly / bluetooth / per-slot) ---

// FriendlyName reads the user-set friendly name.
func (o *Oasis) FriendlyName() (string, error) { return o.readName(opGetFriendlyName, nil) }

// SetFriendlyName writes the user "friendly" name as a fixed 32-byte NUL-padded field
// (opcode 0x05).
func (o *Oasis) SetFriendlyName(name string) error {
	return o.setName32(opSetFriendlyName, name)
}

// BluetoothName reads the device's bluetooth name.
func (o *Oasis) BluetoothName() (string, error) { return o.readName(opGetBluetoothName, nil) }

// SetBluetoothName writes the device's bluetooth name.
func (o *Oasis) SetBluetoothName(name string) error {
	return o.setName32(opSetBluetoothName, name)
}

// setName32 writes a FIXED 32-byte NUL-padded name field — the framing the friendly /
// bluetooth name writes use (payloadLen 0x20; a variable-length payload gets no reply
// from the device, hardware-confirmed). Names longer than 32 bytes are truncated.
func (o *Oasis) setName32(opcode byte, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	nb := make([]byte, 32)
	copy(nb, name)
	_, err := o.command(opcode, nb)
	return err
}

// slotNameLen is the per-slot name field width. Slot-name commands carry a fixed
// [slot]+16-byte payload (17 bytes, descriptor length 0x11); the reply echoes the slot
// at [2] and the 16-byte name from [3]. A variable-length name payload gets no reply.
const slotNameLen = 16

// slotNameField builds the fixed [slot, name padded to 16] slot-name payload. The
// wire slot is 1-based — the same +1 convention as SetPosition (the device numbers
// physical positions 1..N; position 0 is unused). The caller passes a 0-based ASCOM
// slot, so reading/writing ASCOM slot i addresses wire slot i+1. Without this the
// name table reads one slot low: ASCOM slot 0 hit the empty wire slot 0 and every
// real name came back shifted up by one (hardware-confirmed against an LRGB+Ha wheel).
func slotNameField(slot int, name string) []byte {
	p := make([]byte, 1+slotNameLen)
	p[0] = byte(slot + 1) // wire is 1-based
	copy(p[1:], name)
	return p
}

// slotNameAt extracts the 16-byte name (NUL-trimmed) from a slot-name reply: the name
// follows the opcode echo, length, and echoed slot byte (data offset 3).
func slotNameAt(r []byte) (string, error) {
	if len(r) < tableDataOff {
		return "", errors.New("oasis: short slot-name reply")
	}
	end := tableDataOff + slotNameLen
	if end > len(r) {
		end = len(r)
	}
	return trimName(r[tableDataOff:end]), nil
}

// SlotName reads the name of a 0-based filter slot.
func (o *Oasis) SlotName(slot int) (string, error) {
	if slot < 0 || slot > 0xff {
		return "", fmt.Errorf("oasis: slot %d out of range", slot)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opGetSlotName, slotNameField(slot, ""))
	if err != nil {
		return "", err
	}
	return slotNameAt(r)
}

// SetSlotName writes the name of a 0-based filter slot (truncated to 16 bytes).
func (o *Oasis) SetSlotName(slot int, name string) error {
	if slot < 0 || slot > 0xff {
		return fmt.Errorf("oasis: slot %d out of range", slot)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opSetSlotName, slotNameField(slot, name))
	return err
}

// --- Per-slot focus offsets & colors (8-entry int32 big-endian tables) ---
//
// each is a whole page of 8 big-endian
// int32 entries, indexed by slot. The command payload is [page, 0x00*32]
// (descriptor byte[1] = 0x21 = 33), and the reply carries the 8 int32 starting at
// data offset 3, each run through ntohl. The SET-page frame mirrors the GET page —
const (
	tableEntries  = 8                  // entries per page
	tableDataOff  = 3                  // reply offset where the int32 table begins
	tablePayloadN = 1 + tableEntries*4 // 33: [page] + 8 * int32
)

// getTableEntry reads one slot from an 8-entry int32 BE page table. The wire slot is
// 1-based (slot 0 unused), matching the slot-name and SetPosition commands, so the
// 0-based ASCOM slot i is stored at flat wire index i+1.
func (o *Oasis) getTableEntry(getOp byte, slot int) (uint32, error) {
	if slot < 0 || slot > 0xff {
		return 0, fmt.Errorf("oasis: slot %d out of range", slot)
	}
	ws := slot + 1 // wire is 1-based
	pl := make([]byte, tablePayloadN)
	pl[0] = byte(ws / tableEntries) // page
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(getOp, pl)
	if err != nil {
		return 0, err
	}
	off := tableDataOff + (ws%tableEntries)*4
	if len(r) < off+4 {
		return 0, errors.New("oasis: short table reply")
	}
	return binary.BigEndian.Uint32(r[off : off+4]), nil
}

// setTableEntry does a read-modify-write of the slot's page (the SET frame mirrors GET;
func (o *Oasis) setTableEntry(getOp, setOp byte, slot int, v uint32) error {
	if slot < 0 || slot > 0xff {
		return fmt.Errorf("oasis: slot %d out of range", slot)
	}
	ws := slot + 1 // wire is 1-based (see getTableEntry)
	page := byte(ws / tableEntries)
	rd := make([]byte, tablePayloadN)
	rd[0] = page
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(getOp, rd) // read current page
	if err != nil {
		return err
	}
	if len(r) < tableDataOff+tableEntries*4 {
		return errors.New("oasis: short table reply")
	}
	wpl := make([]byte, tablePayloadN)
	wpl[0] = page
	copy(wpl[1:], r[tableDataOff:tableDataOff+tableEntries*4])
	binary.BigEndian.PutUint32(wpl[1+(ws%tableEntries)*4:], v)
	_, err = o.command(setOp, wpl)
	return err
}

// readTable reads n slots from an 8-per-page int32 BE table (one command per page),
// e.g. all slots' focus offsets or colors, mapped onto the 0-based ASCOM slot array.
// Wire slots are 1-based (slot 0 unused — see getTableEntry), so ASCOM slot i is at
// flat wire index i+1; this reads flat indices 1..n. Caller holds mu.
func (o *Oasis) readTable(getOp byte, n int) ([]uint32, error) {
	out := make([]uint32, 0, n)
	for flat := 1; len(out) < n; { // flat wire index, 1-based
		page := byte(flat / tableEntries)
		pl := make([]byte, tablePayloadN)
		pl[0] = page
		r, err := o.command(getOp, pl)
		if err != nil {
			return nil, err
		}
		if len(r) < tableDataOff+tableEntries*4 {
			return nil, errors.New("oasis: short table reply")
		}
		for i := flat % tableEntries; i < tableEntries && len(out) < n; i++ {
			off := tableDataOff + i*4
			out = append(out, binary.BigEndian.Uint32(r[off:off+4]))
			flat++
		}
	}
	return out, nil
}

// FocusOffset reads a slot's focus offset (signed int32, big-endian on the wire).
func (o *Oasis) FocusOffset(slot int) (int32, error) {
	u, err := o.getTableEntry(opGetFocusOffset, slot)
	return int32(u), err
}

// FocusOffsets reads every slot's focus offset in one pass. The device returns the
// page table whole, so this is one round-trip per 8 slots (vs N per-slot calls) and
// maps directly onto the ASCOM FocusOffsets array.
func (o *Oasis) FocusOffsets() ([]int32, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.slotsLocked()
	if err != nil {
		return nil, err
	}
	u, err := o.readTable(opGetFocusOffset, n)
	if err != nil {
		return nil, err
	}
	out := make([]int32, len(u))
	for i, v := range u {
		out[i] = int32(v)
	}
	return out, nil
}

// Colors reads every slot's color in one pass (see FocusOffsets).
func (o *Oasis) Colors() ([]uint32, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.slotsLocked()
	if err != nil {
		return nil, err
	}
	return o.readTable(opGetColor, n)
}

// Names reads every slot's filter name (one command per slot), mapping onto the
// ASCOM FilterWheel Names array.
func (o *Oasis) Names() ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.slotsLocked()
	if err != nil {
		return nil, err
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		r, err := o.command(opGetSlotName, slotNameField(i, ""))
		if err != nil {
			return nil, err
		}
		name, err := slotNameAt(r)
		if err != nil {
			return nil, err
		}
		out[i] = name
	}
	return out, nil
}

// SetFocusOffset writes a slot's focus offset (read-modify-write of its page).
func (o *Oasis) SetFocusOffset(slot int, offset int32) error {
	return o.setTableEntry(opGetFocusOffset, opSetFocusOffset, slot, uint32(offset))
}

// Color reads a slot's indicator color, packed 0xAARRGGBB (ARGB — alpha high byte;
// hardware-confirmed 2026-06-09 by a byte-identical 0xff112233 round-trip).
func (o *Oasis) Color(slot int) (uint32, error) {
	return o.getTableEntry(opGetColor, slot)
}

// SetColor writes a slot's indicator color (read-modify-write of its page).
func (o *Oasis) SetColor(slot int, color uint32) error {
	return o.setTableEntry(opGetColor, opSetColor, slot, color)
}

// --- internal helpers ---

// readRaw issues a no-payload read command and returns its reply payload (reply[2:]).
// Caller holds mu.
func (o *Oasis) readRaw(opcode byte) ([]byte, error) {
	r, err := o.command(opcode, nil)
	if err != nil {
		return nil, err
	}
	if len(r) < 2 {
		return nil, fmt.Errorf("oasis: short reply to 0x%02x", opcode)
	}
	// Return the DECLARED payload (reply[1] bytes), not the trailing report bytes — the read
	// yields a full 64-byte report. This keeps raw blocks clean and round-trippable into the
	// matching write (e.g. ConfigRaw → SetConfigRaw expects exactly the block length).
	end := 2 + int(r[1])
	if end > len(r) {
		end = len(r)
	}
	return append([]byte(nil), r[2:end]...), nil
}

// readName issues a (possibly slot-indexed) read and returns the reply payload as a
// NUL-trimmed string.
func (o *Oasis) readName(opcode byte, payload []byte) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opcode, payload)
	if err != nil {
		return "", err
	}
	if len(r) < 2 {
		return "", fmt.Errorf("oasis: short reply to 0x%02x", opcode)
	}
	return trimName(r[2:]), nil
}

// trimName returns b up to the first NUL byte.
func trimName(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
