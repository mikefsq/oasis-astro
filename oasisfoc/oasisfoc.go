package oasisfoc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// --- Oasis focuser HID framing. Identical transport and framing to the Oasis filter
// wheel (oasisfw): a 65-byte interrupt report.
//
//	command : [0]=0x00 reportID  [1]=opcode  [2]=payloadLen  [3..]=payload (padded to 65)
//	reply   : [0]=opcode echo    [1]=len     [2..]=data
//
// Multi-byte numeric fields are BIG-ENDIAN.
// The transceiver drains stale IN reports, writes, then reads the reply.
const (
	reportLen   = 65
	reportID    = 0x00
	replyWaitMS = 100

	// A transient HID write failure (USB power-management wake on a Full-Speed device,
	// a hub transaction-translator stall, or a brief IOKit hiccup) is retried before the
	// device is declared lost — a single rejected SetReport must not tear the handle down.
	// Safe to resend: a failed IOHIDDeviceSetReport did not deliver the report, so the
	// device still receives the command exactly once (on the attempt that succeeds).
	writeTries        = 3
	writeRetryBackoff = 8 * time.Millisecond

	// Opcodes.
	opSerial           = 0x03 // factory serial (read)
	opGetFriendlyName  = 0x04 // user friendly name (read; 32-byte field)
	opSetFriendlyName  = 0x05 // user friendly name (write; fixed 32-byte field)
	opGetBluetoothName = 0x06 // bluetooth name (read)
	opSetBluetoothName = 0x07 // bluetooth name (write)
	opStatus           = 0x32 // status: temps, position, moving (read)
	opFactoryReset     = 0x33 // restore factory defaults (write; confirmed)
	opSetZeroPosition  = 0x34 // define current position as zero (write)
	opMove             = 0x35 // relative move; payload [dir, htonl(int32 steps)] (confirmed)
	opMoveTo           = 0x36 // absolute move; payload htonl(int32 target) (confirmed)
	opStopMove         = 0x37 // halt motion (write)
	opSyncPosition     = 0x38 // set the reported position without moving; payload int32 BE (write)
	opClearStall       = 0x3d // clear a stall condition (write)
	opInfoA            = 0x02 // version (reply 36B = version fields + build-date string)
	opInfoB            = 0x01 // product model (reply 32B NUL-terminated, e.g. "OasisFocuserRose")

	opConfig       = 0x30 // device config block, part 1 (read)
	opSetConfig    = 0x31 // write part-1 config (payload 18 bytes)
	opConfigExt    = 0x3a // extended config block, part 2 (read; heating/stall/usbPower)
	opSetConfigExt = 0x3b // write part-2/ext config (payload 40 bytes)
)

// Status reply layout (opcode 0x32, 14 data bytes). Offsets are relative to
// reply[0] (the opcode echo).
const (
	stTempIntOff = 2  // internal-thermistor raw ADC: int32 big-endian
	stExtTempOff = 8  // external temperature: int16 big-endian, 1/16 °C
	stTempDetOff = 10 // temperatureDetection: byte
	stMovingOff  = 11 // moving: byte (0 = idle)
	stPosOff     = 12 // position: int32 big-endian
)

// Oasis is an opened focuser.
type Oasis struct {
	t    Transport
	info DeviceInfo

	mu sync.Mutex // serializes a command + its reply per device

	infoA, infoB []byte // cached identify-handshake replies (see VersionRaw/ModelRaw)
}

// New wraps an already-open Transport as a focuser handle. Most callers use
// OpenFirst / OpenAt; New is for a custom Transport (alternate backend, or a fake
// for testing without hardware).
func New(t Transport, info DeviceInfo) *Oasis { return &Oasis{t: t, info: info} }

// OpenFirst finds and opens the first attached Oasis focuser.
func OpenFirst() (*Oasis, error) {
	t, info, err := openFirst()
	if err != nil {
		return nil, err
	}
	o := New(t, info)
	o.Handshake() // best-effort identify
	return o, nil
}

// OpenAt opens the focuser at a specific USB locationID (from Enumerate).
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
// [0x00, opcode, len, payload...], then read the reply. Caller holds mu.
func (o *Oasis) command(opcode byte, payload []byte) ([]byte, error) {
	if len(payload)+2 > reportLen-1 {
		return nil, fmt.Errorf("oasisfoc: payload too long (%d)", len(payload))
	}
	scratch := make([]byte, reportLen)
	for {
		n, err := o.t.Read(scratch, 0) // drain stale IN reports
		if err != nil || n <= 0 {
			break
		}
	}
	frame := make([]byte, reportLen)
	frame[0], frame[1], frame[2] = reportID, opcode, byte(len(payload))
	copy(frame[3:], payload)
	var werr error
	for try := 0; try < writeTries; try++ {
		if _, werr = o.t.Write(frame); werr == nil {
			break
		}
		time.Sleep(writeRetryBackoff) // transient stall — brief backoff, then resend
	}
	if werr != nil {
		return nil, fmt.Errorf("oasisfoc: write opcode 0x%02x (after %d tries): %w", opcode, writeTries, werr)
	}

	// Read the reply, SKIPPING any stale report whose echo != our opcode. The interrupt-IN
	// drain above is racy — a reply to a previous command can land in the queue after the
	// drain and before our reply, so the first report read may be that leftover. Discard
	// mismatches and read the next, bounded so a genuinely silent opcode still fails promptly.
	for attempts := 0; attempts < 8; attempts++ {
		reply := make([]byte, reportLen)
		n, err := o.t.Read(reply, replyWaitMS)
		if err != nil {
			return nil, fmt.Errorf("oasisfoc: read reply to 0x%02x: %w", opcode, err)
		}
		if n == 0 {
			return nil, fmt.Errorf("oasisfoc: no reply to 0x%02x", opcode)
		}
		if reply[0] == opcode {
			return reply[:n], nil
		}
		// stale reply (echo != ours) — discard and read the next report
	}
	return nil, fmt.Errorf("oasisfoc: no matching reply to 0x%02x (only stale reports)", opcode)
}

// Command issues a raw command and returns the raw reply, for hardware debugging.
func (o *Oasis) Command(opcode byte, payload []byte) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.command(opcode, payload)
}

func (o *Oasis) status() ([]byte, error) { return o.command(opStatus, nil) }

// Status returns the raw status reply, for debugging.
func (o *Oasis) Status() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status()
}

// Position returns the current absolute position (signed step count; big-endian
// int32 on the wire).
func (o *Oasis) Position() (int32, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.status()
	if err != nil {
		return 0, err
	}
	if len(r) < stPosOff+4 {
		return 0, errors.New("oasisfoc: short status reply")
	}
	return int32(binary.BigEndian.Uint32(r[stPosOff : stPosOff+4])), nil
}

// Moving reports whether the focuser is in motion.
func (o *Oasis) Moving() (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.status()
	if err != nil {
		return false, err
	}
	if len(r) <= stMovingOff {
		return false, errors.New("oasisfoc: short status reply")
	}
	return r[stMovingOff] != 0, nil
}

// TemperatureExternal returns the external probe temperature in °C. The wire field
// is a signed int16 in 1/16 °C, so this is exact.
func (o *Oasis) TemperatureExternal() (float64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.status()
	if err != nil {
		return 0, err
	}
	if len(r) < stExtTempOff+2 {
		return 0, errors.New("oasisfoc: short status reply")
	}
	v := int16(binary.BigEndian.Uint16(r[stExtTempOff : stExtTempOff+2]))
	return float64(v) / 16.0, nil
}

// TemperatureInternalRaw returns the raw internal-thermistor ADC value.
func (o *Oasis) TemperatureInternalRaw() (int32, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.status()
	if err != nil {
		return 0, err
	}
	if len(r) < stTempIntOff+4 {
		return 0, errors.New("oasisfoc: short status reply")
	}
	return int32(binary.BigEndian.Uint32(r[stTempIntOff : stTempIntOff+4])), nil
}

// TemperatureInternal converts the raw internal-thermistor ADC to °C using
// a standard 10k NTC (β=3380, T0=25 °C) curve.
func (o *Oasis) TemperatureInternal() (float64, error) {
	raw, err := o.TemperatureInternalRaw()
	if err != nil {
		return 0, err
	}
	adc := float64(raw)
	if adc < 1 {
		adc = 1
	} else if adc > 4094 {
		adc = 4094
	}
	const (
		bOverT0 = 11.3367
		beta    = 3380.0
		kelvin  = 273.15
	)
	return beta/(math.Log((4095.0-adc)/adc)+bOverT0) - kelvin, nil
}

// MoveTo commands an absolute move to the given position (big-endian int32 on the
// wire). Returns once the command is acknowledged; poll Moving/Position for
// completion.
func (o *Oasis) MoveTo(position int32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, uint32(position))
	_, err := o.command(opMoveTo, p)
	return err
}

// Move commands a relative move. Payload is [dir, big-endian int32 magnitude]; dir is
// passed straight through as payload[0] (it is NOT derived from a signed step).
func (o *Oasis) Move(dir int, steps int32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := make([]byte, 5)
	p[0] = byte(dir)
	binary.BigEndian.PutUint32(p[1:], uint32(steps))
	_, err := o.command(opMove, p)
	return err
}

// MoveOut moves the drawtube OUT by steps — increasing position (dir 1).
// flips if ReverseDirection is set.
func (o *Oasis) MoveOut(steps int32) error { return o.Move(1, steps) }

// MoveIn moves the drawtube IN by steps — decreasing position (dir 0).
// flips if ReverseDirection is set.
func (o *Oasis) MoveIn(steps int32) error { return o.Move(0, steps) }

// StopMove halts any in-progress motion (ASCOM Halt).
func (o *Oasis) StopMove() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opStopMove, nil)
	return err
}

// SyncPosition sets the reported position to the given value without moving
func (o *Oasis) SyncPosition(position int32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, uint32(position))
	_, err := o.command(opSyncPosition, p)
	return err
}

// SetZeroPosition defines the current position as zero.
func (o *Oasis) SetZeroPosition() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opSetZeroPosition, nil)
	return err
}

// ClearStall clears a latched stall condition.
func (o *Oasis) ClearStall() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opClearStall, nil)
	return err
}

// SerialRaw returns the raw serial-number reply payload.
func (o *Oasis) SerialRaw() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opSerial, nil)
	if err != nil {
		return nil, err
	}
	if len(r) < 2 {
		return nil, errors.New("oasisfoc: short serial reply")
	}
	return append([]byte(nil), r[2:]...), nil
}

// Config mirrors part 1 of the device's config block (opcode 0x30).
type Config struct {
	Mask              uint32
	MaxStep           int32 // ASCOM MaxStep / MaxIncrement
	Backlash          int32
	BacklashDirection int
	ReverseDirection  int
	Speed             int
	BeepOnMove        int
	BeepOnStartup     int
	BluetoothOn       int
}

// Config reads and decodes the focuser config (part 1).
func (o *Oasis) Config() (Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opConfig, nil)
	if err != nil {
		return Config{}, err
	}
	if len(r) < 20 {
		return Config{}, errors.New("oasisfoc: short config reply")
	}
	be := binary.BigEndian
	return Config{
		Mask:              be.Uint32(r[2:6]),
		MaxStep:           int32(be.Uint32(r[6:10])),
		Backlash:          int32(be.Uint32(r[10:14])),
		BacklashDirection: int(r[14]),
		ReverseDirection:  int(r[15]),
		Speed:             int(r[16]),
		BeepOnMove:        int(r[17]),
		BeepOnStartup:     int(r[18]),
		BluetoothOn:       int(r[19]),
	}, nil
}

// MaxStep returns the focuser's maximum step (ASCOM MaxStep / MaxIncrement).
func (o *Oasis) MaxStep() (int32, error) {
	c, err := o.Config()
	return c.MaxStep, err
}

// SetConfig writes the part-1 config block. Payload mirrors the 0x30 read layout:
func (o *Oasis) SetConfig(c Config) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := make([]byte, 18)
	be := binary.BigEndian
	be.PutUint32(p[0:4], c.Mask)
	be.PutUint32(p[4:8], uint32(c.MaxStep))
	be.PutUint32(p[8:12], uint32(c.Backlash))
	p[12] = byte(c.BacklashDirection)
	p[13] = byte(c.ReverseDirection)
	p[14] = byte(c.Speed)
	p[15] = byte(c.BeepOnMove)
	p[16] = byte(c.BeepOnStartup)
	p[17] = byte(c.BluetoothOn)
	_, err := o.command(opSetConfig, p)
	return err
}

// modifyConfig read-modify-writes a single part-1 field: it reads the current config,
// applies fn, and writes it back with the device's own mask so only that field changes.
func (o *Oasis) modifyConfig(fn func(*Config)) error {
	c, err := o.Config()
	if err != nil {
		return err
	}
	fn(&c)
	return o.SetConfig(c)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SetBeepOnMove enables/disables the beep on each move.
func (o *Oasis) SetBeepOnMove(on bool) error {
	return o.modifyConfig(func(c *Config) { c.BeepOnMove = b2i(on) })
}

// SetBeepOnStartup enables/disables the power-up beep.
func (o *Oasis) SetBeepOnStartup(on bool) error {
	return o.modifyConfig(func(c *Config) { c.BeepOnStartup = b2i(on) })
}

// SetReverseDirection sets motion-direction reversal (ASCOM Reverse).
func (o *Oasis) SetReverseDirection(on bool) error {
	return o.modifyConfig(func(c *Config) { c.ReverseDirection = b2i(on) })
}

// SetBacklash sets the backlash compensation step count.
func (o *Oasis) SetBacklash(steps int32) error {
	return o.modifyConfig(func(c *Config) { c.Backlash = steps })
}

// SetBacklashDirection sets the backlash compensation direction.
func (o *Oasis) SetBacklashDirection(dir int) error {
	return o.modifyConfig(func(c *Config) { c.BacklashDirection = dir })
}

// SetSpeed sets the motor speed setting.
func (o *Oasis) SetSpeed(speed int) error {
	return o.modifyConfig(func(c *Config) { c.Speed = speed })
}

// SetMaxStep sets the focuser's maximum-step travel limit.
func (o *Oasis) SetMaxStep(max int32) error {
	return o.modifyConfig(func(c *Config) { c.MaxStep = max })
}

// ExtConfig is the part-2 config block (opcode 0x3a read / 0x3b write): the part-1
// fields plus the dew-heater / stall-detection / USB-power extension.
// HeatingTemperature is the device's raw units (observed 2500 ≈ 25.00 °C, i.e. centi-°C).
type ExtConfig struct {
	Config
	StallDetection     int
	HeatingTemperature int32
	HeatingOn          int
	UsbPowerCapacity   int32
}

// ExtConfig reads and decodes the extended config (part 2, opcode 0x3a).
func (o *Oasis) ExtConfig() (ExtConfig, error) {
	raw, err := o.ExtConfigRaw()
	if err != nil {
		return ExtConfig{}, err
	}
	if len(raw) < 28 {
		return ExtConfig{}, errors.New("oasisfoc: short ext-config reply")
	}
	be := binary.BigEndian
	e := ExtConfig{Config: Config{
		Mask:              be.Uint32(raw[0:4]),
		MaxStep:           int32(be.Uint32(raw[4:8])),
		Backlash:          int32(be.Uint32(raw[8:12])),
		BacklashDirection: int(raw[12]),
		ReverseDirection:  int(raw[13]),
		Speed:             int(raw[14]),
		BeepOnMove:        int(raw[15]),
		BeepOnStartup:     int(raw[16]),
		BluetoothOn:       int(raw[17]),
	}}
	e.StallDetection = int(raw[18])
	e.HeatingTemperature = int32(be.Uint32(raw[19:23]))
	e.HeatingOn = int(raw[23])
	e.UsbPowerCapacity = int32(be.Uint32(raw[24:28]))
	return e, nil
}

// patchExtConfig read-modify-writes the part-2 block at the BYTE level (opcode 0x3b),
// so reserved bytes and every unread field are preserved exactly — only the patched
// bytes change. The part-2 write block is 40 bytes.
func (o *Oasis) patchExtConfig(patch func(raw []byte)) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opConfigExt, nil)
	if err != nil {
		return err
	}
	if len(r) < 2+28 {
		return errors.New("oasisfoc: short ext-config reply")
	}
	end := 2 + 40
	if end > len(r) {
		end = len(r)
	}
	raw := append([]byte(nil), r[2:end]...)
	patch(raw)
	_, err = o.command(opSetConfigExt, raw)
	return err
}

// SetHeatingOn enables/disables the dew heater (block byte @23).
func (o *Oasis) SetHeatingOn(on bool) error {
	return o.patchExtConfig(func(raw []byte) { raw[23] = byte(b2i(on)) })
}

// SetHeatingTemperature sets the dew-heater target in the device's raw units
// (centi-°C; e.g. 2500 = 25.00 °C). Block bytes @19:23 (BE int32).
func (o *Oasis) SetHeatingTemperature(centiC int32) error {
	return o.patchExtConfig(func(raw []byte) { binary.BigEndian.PutUint32(raw[19:23], uint32(centiC)) })
}

// SetStallDetection enables/disables stall detection (block byte @18).
func (o *Oasis) SetStallDetection(on bool) error {
	return o.patchExtConfig(func(raw []byte) { raw[18] = byte(b2i(on)) })
}

// SetUsbPowerCapacity sets the USB power-capacity budget.
func (o *Oasis) SetUsbPowerCapacity(v int32) error {
	return o.patchExtConfig(func(raw []byte) { binary.BigEndian.PutUint32(raw[24:28], uint32(v)) })
}

// ConfigRaw returns the raw part-1 config reply (reply[2:]), for debugging.
func (o *Oasis) ConfigRaw() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opConfig, nil)
	if err != nil {
		return nil, err
	}
	if len(r) < 2 {
		return nil, errors.New("oasisfoc: short config reply")
	}
	return append([]byte(nil), r[2:]...), nil
}

// ExtConfigRaw returns the raw part-2 config reply.
func (o *Oasis) ExtConfigRaw() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opConfigExt, nil)
	if err != nil {
		return nil, err
	}
	if len(r) < 2 {
		return nil, errors.New("oasisfoc: short ext-config reply")
	}
	return append([]byte(nil), r[2:]...), nil
}

// Handshake issues the identify commands and caches their raw replies
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

// VersionRaw / ModelRaw return the cached handshake replies (the version block and
// product-model string). Prefer the typed
// Serial/Model/HardwareVersion/FirmwareVersion/FirmwareBuildDate accessors.
func (o *Oasis) VersionRaw() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.infoA...)
}

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

// Model returns the product model string (the 0x01 handshake reply).
func (o *Oasis) Model() string {
	b := o.ModelRaw()
	if len(b) < 2 {
		return ""
	}
	return cstr(b[2:])
}

// The version reply.
const (
	verProtocol = 0
	verHardware = 4
	verFirmware = 8
	verBuilt    = 12
)

// versionField returns the 4 version bytes at data offset off (past the echo+len header).
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

// ProtocolVersion returns the wire-protocol version as "major.minor.patch.build".
func (o *Oasis) ProtocolVersion() string { return ver4(o.versionField(verProtocol)) }

// HardwareVersion returns the hardware version as "major.minor.patch.build" (e.g. "1.2.0.0").
func (o *Oasis) HardwareVersion() string { return ver4(o.versionField(verHardware)) }

// FirmwareVersion returns the firmware version as "major.minor.patch.build" (e.g. "2.1.1.0").
func (o *Oasis) FirmwareVersion() string { return ver4(o.versionField(verFirmware)) }

// FirmwareBuildDate returns the firmware build timestamp (the vendor app's "Firmware
// Version" field, e.g. "Mar 16 2026, 15:58:02").
func (o *Oasis) FirmwareBuildDate() string {
	v := o.VersionRaw()
	if len(v) < 2+verBuilt {
		return ""
	}
	return cstr(v[2+verBuilt:])
}

// --- Names + factory reset ---

// FriendlyName reads the user friendly name (NUL-terminated, 32-byte field).
func (o *Oasis) FriendlyName() (string, error) { return o.readName(opGetFriendlyName) }

// BluetoothName reads the device's bluetooth name.
func (o *Oasis) BluetoothName() (string, error) { return o.readName(opGetBluetoothName) }

// SetBluetoothName writes the device's bluetooth name (single-command write).
func (o *Oasis) SetBluetoothName(name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	nb := make([]byte, 32)
	copy(nb, name)
	_, err := o.command(opSetBluetoothName, nb)
	return err
}

// SetFriendlyName writes the user "friendly" name as a FIXED 32-byte NUL-padded field.
func (o *Oasis) SetFriendlyName(name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	nb := make([]byte, 32)
	copy(nb, name)
	_, err := o.command(opSetFriendlyName, nb)
	return err
}

// FactoryReset restores the focuser's factory defaults.
func (o *Oasis) FactoryReset() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.command(opFactoryReset, nil)
	return err
}

// readName reads a NUL-terminated name field (reply[2:]).
func (o *Oasis) readName(opcode byte) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.command(opcode, nil)
	if err != nil {
		return "", err
	}
	if len(r) < 2 {
		return "", fmt.Errorf("oasisfoc: short reply to 0x%02x", opcode)
	}
	b := r[2:]
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), nil
		}
	}
	return string(b), nil
}
