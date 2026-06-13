package oasisfoc

import (
	"bytes"
	"sync"
	"testing"
)

// fakeHID is an in-memory Transport: records command frames and returns a canned
// reply keyed by opcode — so the protocol layer is testable with no hardware/cgo.
type fakeHID struct {
	mu      sync.Mutex
	written    [][]byte
	replies    map[byte][]byte
	drained    int
	failW      bool
	failWrites int // fail this many writes (transient), then succeed
}

func newFake() *fakeHID { return &fakeHID{replies: map[byte][]byte{}} }

func (f *fakeHID) Write(buf []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failW {
		return 0, errGone
	}
	if f.failWrites > 0 {
		f.failWrites--
		return 0, errGone
	}
	f.written = append(f.written, append([]byte(nil), buf...))
	return len(buf), nil
}

func (f *fakeHID) Read(buf []byte, timeoutMS int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if timeoutMS <= 0 {
		f.drained++
		return 0, nil
	}
	if len(f.written) == 0 {
		return 0, nil
	}
	if r, ok := f.replies[f.written[len(f.written)-1][1]]; ok {
		return copy(buf, r), nil
	}
	return 0, nil
}

func (f *fakeHID) Close() error { return nil }

func (f *fakeHID) lastWrite() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.written) == 0 {
		return nil
	}
	return f.written[len(f.written)-1]
}

// writeFor returns the most recent command frame written for opcode op (nil if none).
func (f *fakeHID) writeFor(op byte) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.written) - 1; i >= 0; i-- {
		if w := f.written[i]; len(w) > 1 && w[1] == op {
			return w
		}
	}
	return nil
}

type goneErr struct{}

func (goneErr) Error() string { return "device removed" }

var errGone = goneErr{}

func testFoc(f *fakeHID) *Oasis { return New(f, DeviceInfo{VID: VID, PID: PID}) }

// TestWriteRetry: a transient write failure is retried (so a momentary USB stall does
// not surface as a command error), but a persistently failing write still errors.
func TestWriteRetry(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = statusReply(12345, 0)
	f.failWrites = writeTries - 1 // first N-1 writes fail, the last succeeds
	if p, err := testFoc(f).Position(); err != nil || p != 12345 {
		t.Fatalf("Position() = %d, %v; want 12345 after transient write retries", p, err)
	}

	f2 := newFake()
	f2.replies[opStatus] = statusReply(12345, 0)
	f2.failWrites = writeTries // every attempt fails
	if _, err := testFoc(f2).Position(); err == nil {
		t.Error("Position() should error when every write attempt fails")
	}
}

// status reply with position=5000, moving=0 (16 bytes).
func statusReply(pos uint32, moving byte) []byte {
	r := make([]byte, 16)
	r[0], r[1] = opStatus, 0x0e
	r[stMovingOff] = moving
	r[stPosOff+0] = byte(pos >> 24)
	r[stPosOff+1] = byte(pos >> 16)
	r[stPosOff+2] = byte(pos >> 8)
	r[stPosOff+3] = byte(pos)
	return r
}

func TestEncodeCommands(t *testing.T) {
	cases := []struct {
		name string
		do   func(*Oasis)
		want []byte
	}{
		{"status", func(o *Oasis) { o.Status() }, []byte{0x00, opStatus, 0x00}},
		{"moveto 5000", func(o *Oasis) { o.MoveTo(5000) }, []byte{0x00, opMoveTo, 0x04, 0x00, 0x00, 0x13, 0x88}},
		{"move +100", func(o *Oasis) { o.Move(1, 100) }, []byte{0x00, opMove, 0x05, 0x01, 0x00, 0x00, 0x00, 0x64}},
		{"stop", func(o *Oasis) { o.StopMove() }, []byte{0x00, opStopMove, 0x00}},
		{"sync 200", func(o *Oasis) { o.SyncPosition(200) }, []byte{0x00, opSyncPosition, 0x04, 0x00, 0x00, 0x00, 0xc8}},
		{"set zero", func(o *Oasis) { o.SetZeroPosition() }, []byte{0x00, opSetZeroPosition, 0x00}},
		{"clear stall", func(o *Oasis) { o.ClearStall() }, []byte{0x00, opClearStall, 0x00}},
		{"serial", func(o *Oasis) { o.SerialRaw() }, []byte{0x00, opSerial, 0x00}},
		{"factory reset", func(o *Oasis) { o.FactoryReset() }, []byte{0x00, opFactoryReset, 0x00}},
		{"friendly name get", func(o *Oasis) { o.FriendlyName() }, []byte{0x00, opGetFriendlyName, 0x00}},
		{"set bt name", func(o *Oasis) { o.SetBluetoothName("BT") }, []byte{0x00, opSetBluetoothName, 0x20, 'B', 'T'}}, // fixed 32-byte NUL-padded field
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			f.replies = map[byte][]byte{
				opStatus:           statusReply(5000, 0),
				opMoveTo:           {opMoveTo, 0x00},
				opMove:             {opMove, 0x00},
				opStopMove:         {opStopMove, 0x00},
				opSyncPosition:     {opSyncPosition, 0x00},
				opSetZeroPosition:  {opSetZeroPosition, 0x00},
				opClearStall:       {opClearStall, 0x00},
				opSerial:           {opSerial, 0x04, 1, 2, 3, 4},
				opFactoryReset:     {opFactoryReset, 0x00},
				opGetFriendlyName:  {opGetFriendlyName, 0x03, 'F', 'X', 0},
				opSetBluetoothName: {opSetBluetoothName, 0x00},
			}
			c.do(testFoc(f))
			got := f.lastWrite()
			if got == nil {
				t.Fatal("no command written")
			}
			if len(got) != reportLen {
				t.Errorf("frame length %d, want %d", len(got), reportLen)
			}
			if !bytes.HasPrefix(got, c.want) {
				t.Errorf("got % x, want prefix % x", got[:len(c.want)+1], c.want)
			}
		})
	}
}

func TestDrainsBeforeWrite(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = statusReply(0, 0)
	testFoc(f).Status()
	if f.drained == 0 {
		t.Error("expected a drain read before the command write")
	}
}

func TestDecodePositionMoving(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = statusReply(12345, 0)
	o := testFoc(f)
	if p, err := o.Position(); err != nil || p != 12345 {
		t.Fatalf("Position()=%d,%v want 12345", p, err)
	}
	if m, err := o.Moving(); err != nil || m {
		t.Fatalf("Moving()=%v,%v want false", m, err)
	}

	f.replies[opStatus] = statusReply(12345, 1)
	if m, err := o.Moving(); err != nil || !m {
		t.Fatalf("Moving()=%v,%v want true", m, err)
	}
}

func TestDecodeConfig(t *testing.T) {
	f := newFake()
	// [op, len, mask=1, maxStep=100000(0x186A0), backlash=50, bd,rd,speed,bom,bos,bt]
	f.replies[opConfig] = []byte{opConfig, 0x12,
		0, 0, 0, 1,
		0x00, 0x01, 0x86, 0xA0,
		0, 0, 0, 50,
		1, 0, 2, 1, 0, 1}
	c, err := testFoc(f).Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.Mask != 1 || c.MaxStep != 100000 || c.Backlash != 50 ||
		c.BacklashDirection != 1 || c.ReverseDirection != 0 || c.Speed != 2 ||
		c.BeepOnMove != 1 || c.BeepOnStartup != 0 || c.BluetoothOn != 1 {
		t.Fatalf("Config()=%+v", c)
	}
}

func TestDecodeFriendlyName(t *testing.T) {
	f := newFake()
	f.replies[opGetFriendlyName] = []byte{opGetFriendlyName, 0x03, 'F', 'X', 0}
	if s, err := testFoc(f).FriendlyName(); err != nil || s != "FX" {
		t.Fatalf("FriendlyName()=%q,%v want FX", s, err)
	}
}

func TestDecodeTemperature(t *testing.T) {
	f := newFake()
	// external temp = 25.0 °C = 400 in 1/16-°C units (0x0190) at reply[8:10] BE
	r := statusReply(0, 0)
	r[stExtTempOff], r[stExtTempOff+1] = 0x01, 0x90
	f.replies[opStatus] = r
	if c, err := testFoc(f).TemperatureExternal(); err != nil || c != 25.0 {
		t.Fatalf("TemperatureExternal()=%v,%v want 25.0", c, err)
	}
}

func TestReplyOpcodeMismatch(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = []byte{0xFF, 0x00}
	if _, err := testFoc(f).Status(); err == nil {
		t.Error("expected error on reply opcode mismatch")
	}
}

func TestDeviceRemoved(t *testing.T) {
	f := newFake()
	f.failW = true
	if _, err := testFoc(f).Position(); err == nil {
		t.Error("want error on removed device")
	}
}

func TestConcurrentAccess(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = statusReply(100, 0)
	f.replies[opMoveTo] = []byte{opMoveTo, 0x00}
	o := testFoc(f)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				o.Position()
			case 1:
				o.MoveTo(int32(i * 10))
			case 2:
				o.Moving()
			}
		}(i)
	}
	wg.Wait()
}

// staleHID returns one stale report (wrong opcode echo) before the real reply on
// each command, to exercise command()'s stale-reply skip-and-recover loop.
type staleHID struct {
	mu        sync.Mutex
	reply     []byte
	staleSent bool
}

func (s *staleHID) Write(buf []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staleSent = false
	return len(buf), nil
}

func (s *staleHID) Read(buf []byte, timeoutMS int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if timeoutMS <= 0 {
		return 0, nil // drain: nothing queued
	}
	if !s.staleSent {
		s.staleSent = true
		return copy(buf, []byte{0xFF, 0x00}), nil // stale: echo 0xFF != our opcode
	}
	return copy(buf, s.reply), nil
}

func (s *staleHID) Close() error { return nil }

// TestSkipsStaleReply verifies the skip-and-recover behavior added to command():
// a leftover report whose echo != our opcode is discarded, and the next (correct)
// report is returned — so the command still succeeds rather than erroring.
func TestSkipsStaleReply(t *testing.T) {
	s := &staleHID{reply: statusReply(777, 0)}
	o := New(s, DeviceInfo{VID: VID, PID: PID})
	p, err := o.Position()
	if err != nil {
		t.Fatalf("Position() after one stale reply: %v (skip loop should recover)", err)
	}
	if p != 777 {
		t.Fatalf("Position()=%d, want 777", p)
	}
}

// TestConfigReadModifyWrite checks that a single-field setter reads the current
// config, flips only that field, and re-encodes the full 18-byte SetConfig payload.
func TestConfigReadModifyWrite(t *testing.T) {
	f := newFake()
	f.replies[opConfig] = []byte{opConfig, 0x12,
		0, 0, 0, 1, // mask=1
		0x00, 0x01, 0x86, 0xA0, // maxStep=100000
		0, 0, 0, 50, // backlash=50
		1, 0, 2, 1, 0, 1} // bd=1 rd=0 speed=2 bom=1 bos=0 bt=1
	f.replies[opSetConfig] = []byte{opSetConfig, 0x00}

	if err := testFoc(f).SetBeepOnMove(false); err != nil { // flip bom 1->0
		t.Fatal(err)
	}
	w := f.writeFor(opSetConfig)
	if w == nil {
		t.Fatal("no SetConfig command written")
	}
	want := []byte{
		0, 0, 0, 1,
		0x00, 0x01, 0x86, 0xA0,
		0, 0, 0, 50,
		1, 0, 2, 0, 0, 1} // only bom changed: 1 -> 0
	if got := w[3 : 3+len(want)]; !bytes.Equal(got, want) {
		t.Errorf("SetConfig payload = % x, want % x", got, want)
	}
}

// extConfigReply builds a 42-byte ext-config reply (40-byte block) with the named fields.
func extConfigReply() []byte {
	r := make([]byte, 42)
	r[0], r[1] = opConfigExt, 0x28 // op, len=40
	raw := r[2:]
	raw[3] = 1                                                  // mask=1
	raw[4], raw[5], raw[6], raw[7] = 0x00, 0x01, 0x86, 0xA0     // maxStep=100000
	raw[14] = 1                                                 // speed=1
	raw[18] = 1                                                 // stallDetection=1
	raw[19], raw[20], raw[21], raw[22] = 0x00, 0x00, 0x09, 0xC4 // heatingTemp=2500
	raw[23] = 0                                                 // heatingOn=0
	raw[24], raw[25], raw[26], raw[27] = 0x00, 0x00, 0x07, 0xD0 // usbPower=2000
	return r
}

func TestDecodeExtConfig(t *testing.T) {
	f := newFake()
	f.replies[opConfigExt] = extConfigReply()
	e, err := testFoc(f).ExtConfig()
	if err != nil {
		t.Fatal(err)
	}
	if e.Mask != 1 || e.MaxStep != 100000 || e.Speed != 1 ||
		e.StallDetection != 1 || e.HeatingTemperature != 2500 ||
		e.HeatingOn != 0 || e.UsbPowerCapacity != 2000 {
		t.Fatalf("ExtConfig()=%+v", e)
	}
}

// TestPatchExtConfig checks the byte-level read-modify-write: only the heater-on byte
// changes; stall/heatingTemp and the rest of the block are preserved.
func TestPatchExtConfig(t *testing.T) {
	f := newFake()
	f.replies[opConfigExt] = extConfigReply()
	f.replies[opSetConfigExt] = []byte{opSetConfigExt, 0x00}

	if err := testFoc(f).SetHeatingOn(true); err != nil {
		t.Fatal(err)
	}
	w := f.writeFor(opSetConfigExt)
	if w == nil {
		t.Fatal("no SetConfigExt command written")
	}
	raw := w[3:] // payload mirrors the 40-byte block
	if raw[23] != 1 {
		t.Errorf("heatingOn byte = %d, want 1", raw[23])
	}
	if raw[18] != 1 { // stallDetection preserved
		t.Errorf("stallDetection byte = %d, want 1 (preserved)", raw[18])
	}
	if raw[21] != 0x09 || raw[22] != 0xC4 { // heatingTemp preserved
		t.Errorf("heatingTemp bytes = % x, want 09 c4 (preserved)", raw[21:23])
	}
}

// TestTemperatureInternal checks the Beta-curve conversion: an ADC at mid-scale
// (where (4095-adc)/adc ≈ 1) maps to ≈25 °C (T0).
func TestTemperatureInternal(t *testing.T) {
	f := newFake()
	r := statusReply(0, 0)
	r[stTempIntOff], r[stTempIntOff+1] = 0x00, 0x00
	r[stTempIntOff+2], r[stTempIntOff+3] = 0x08, 0x00 // raw ADC = 2048
	f.replies[opStatus] = r
	c, err := testFoc(f).TemperatureInternal()
	if err != nil {
		t.Fatal(err)
	}
	if c < 24.5 || c > 25.5 {
		t.Fatalf("TemperatureInternal()=%.3f, want ≈25.0", c)
	}
}

func TestMoveInOutDirection(t *testing.T) {
	f := newFake()
	f.replies[opMove] = []byte{opMove, 0x00}
	o := testFoc(f)

	o.MoveOut(100) // dir 1
	if w := f.writeFor(opMove); w == nil || w[3] != 1 {
		t.Errorf("MoveOut dir byte = %v, want 1", w)
	}
	o.MoveIn(100) // dir 0
	if w := f.lastWrite(); w[1] != opMove || w[3] != 0 {
		t.Errorf("MoveIn dir byte = %v, want 0", w)
	}
}

func TestDecodeSerial(t *testing.T) {
	f := newFake()
	f.replies[opSerial] = []byte{opSerial, 0x04, 'A', 'B', 'C', 0}
	if s, err := testFoc(f).Serial(); err != nil || s != "ABC" {
		t.Fatalf("Serial()=%q,%v want ABC", s, err)
	}
}

// TestHandshakeVersion feeds the two identify replies and checks the parsed
// version/model accessors (three 4-byte version fields + a NUL-terminated build date).
func TestHandshakeVersion(t *testing.T) {
	f := newFake()
	ver := []byte{opInfoA, 0x18,
		1, 0, 0, 0, // protocol 1.0.0.0
		1, 2, 0, 0, // hardware 1.2.0.0
		2, 1, 1, 0} // firmware 2.1.1.0
	ver = append(ver, []byte("Mar 16 2026")...)
	ver = append(ver, 0)
	f.replies[opInfoA] = ver
	f.replies[opInfoB] = append([]byte{opInfoB, 0x0d}, append([]byte("OasisFocuser"), 0)...)

	o := testFoc(f)
	if err := o.Handshake(); err != nil {
		t.Fatal(err)
	}
	if got := o.ProtocolVersion(); got != "1.0.0.0" {
		t.Errorf("ProtocolVersion()=%q, want 1.0.0.0", got)
	}
	if got := o.HardwareVersion(); got != "1.2.0.0" {
		t.Errorf("HardwareVersion()=%q, want 1.2.0.0", got)
	}
	if got := o.FirmwareVersion(); got != "2.1.1.0" {
		t.Errorf("FirmwareVersion()=%q, want 2.1.1.0", got)
	}
	if got := o.FirmwareBuildDate(); got != "Mar 16 2026" {
		t.Errorf("FirmwareBuildDate()=%q, want \"Mar 16 2026\"", got)
	}
	if got := o.Model(); got != "OasisFocuser" {
		t.Errorf("Model()=%q, want OasisFocuser", got)
	}
}

// cfgReply is a valid part-1 config block reply: mask=1, maxStep=100000, backlash=50,
// bd=1, rd=0, speed=2, bom=1, bos=0, bt=1.
func cfgReply() []byte {
	return []byte{opConfig, 0x12,
		0, 0, 0, 1,
		0x00, 0x01, 0x86, 0xA0,
		0, 0, 0, 50,
		1, 0, 2, 1, 0, 1}
}

// TestPart1Setters covers every typed part-1 setter: each reads the config, changes
// only its field, and writes the 18-byte SetConfig payload. Payload offsets: maxStep@4,
// backlash@8, backlashDir@12, reverse@13, speed@14, beepOnStartup@16.
func TestPart1Setters(t *testing.T) {
	cases := []struct {
		name string
		do   func(*Oasis) error
		idx  int
		want []byte
	}{
		{"beepOnStartup", func(o *Oasis) error { return o.SetBeepOnStartup(true) }, 16, []byte{1}},
		{"reverse", func(o *Oasis) error { return o.SetReverseDirection(true) }, 13, []byte{1}},
		{"backlash", func(o *Oasis) error { return o.SetBacklash(99) }, 8, []byte{0, 0, 0, 99}},
		{"backlashDir", func(o *Oasis) error { return o.SetBacklashDirection(0) }, 12, []byte{0}},
		{"speed", func(o *Oasis) error { return o.SetSpeed(5) }, 14, []byte{5}},
		{"maxStep", func(o *Oasis) error { return o.SetMaxStep(200000) }, 4, []byte{0x00, 0x03, 0x0D, 0x40}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			f.replies[opConfig] = cfgReply()
			f.replies[opSetConfig] = []byte{opSetConfig, 0x00}
			if err := c.do(testFoc(f)); err != nil {
				t.Fatal(err)
			}
			w := f.writeFor(opSetConfig)
			if w == nil {
				t.Fatal("no SetConfig written")
			}
			if got := w[3+c.idx : 3+c.idx+len(c.want)]; !bytes.Equal(got, c.want) {
				t.Errorf("payload[%d:] = % x, want % x", c.idx, got, c.want)
			}
		})
	}
}

// TestExtSetters covers the byte-level ext-config setters: each patches only its
// field in the 40-byte block. Block offsets: stall@18, heatingTemp@19, usbPower@24.
func TestExtSetters(t *testing.T) {
	cases := []struct {
		name string
		do   func(*Oasis) error
		idx  int
		want []byte
	}{
		{"heatingTemp", func(o *Oasis) error { return o.SetHeatingTemperature(3000) }, 19, []byte{0x00, 0x00, 0x0B, 0xB8}},
		{"stallOff", func(o *Oasis) error { return o.SetStallDetection(false) }, 18, []byte{0}},
		{"usbPower", func(o *Oasis) error { return o.SetUsbPowerCapacity(1500) }, 24, []byte{0x00, 0x00, 0x05, 0xDC}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			f.replies[opConfigExt] = extConfigReply()
			f.replies[opSetConfigExt] = []byte{opSetConfigExt, 0x00}
			if err := c.do(testFoc(f)); err != nil {
				t.Fatal(err)
			}
			w := f.writeFor(opSetConfigExt)
			if w == nil {
				t.Fatal("no SetConfigExt written")
			}
			if got := w[3+c.idx : 3+c.idx+len(c.want)]; !bytes.Equal(got, c.want) {
				t.Errorf("block[%d:] = % x, want % x", c.idx, got, c.want)
			}
		})
	}
}

// TestMaxStepRawCommand covers MaxStep, ConfigRaw, and the Command passthrough.
func TestMaxStepRawCommand(t *testing.T) {
	f := newFake()
	f.replies[opConfig] = cfgReply()
	f.replies[opStatus] = statusReply(0, 0)
	o := testFoc(f)
	if m, err := o.MaxStep(); err != nil || m != 100000 {
		t.Fatalf("MaxStep()=%d,%v want 100000", m, err)
	}
	if r, err := o.ConfigRaw(); err != nil || len(r) == 0 {
		t.Fatalf("ConfigRaw()=%v,%v want non-empty", r, err)
	}
	if _, err := o.Command(opStatus, nil); err != nil {
		t.Errorf("Command(status) err = %v", err)
	}
}

// TestErrorsPropagate: a removed device (every Write fails) must surface an error
// from every accessor and setter, never a zero value or panic.
func TestErrorsPropagate(t *testing.T) {
	f := newFake()
	f.failW = true
	o := testFoc(f)
	checks := map[string]func() error{
		"Position":               func() error { _, e := o.Position(); return e },
		"Moving":                 func() error { _, e := o.Moving(); return e },
		"TemperatureExternal":    func() error { _, e := o.TemperatureExternal(); return e },
		"TemperatureInternalRaw": func() error { _, e := o.TemperatureInternalRaw(); return e },
		"TemperatureInternal":    func() error { _, e := o.TemperatureInternal(); return e },
		"Config":                 func() error { _, e := o.Config(); return e },
		"ExtConfig":              func() error { _, e := o.ExtConfig(); return e },
		"ConfigRaw":              func() error { _, e := o.ConfigRaw(); return e },
		"ExtConfigRaw":           func() error { _, e := o.ExtConfigRaw(); return e },
		"Serial":                 func() error { _, e := o.Serial(); return e },
		"MaxStep":                func() error { _, e := o.MaxStep(); return e },
		"Handshake":              o.Handshake,
		"MoveTo":                 func() error { return o.MoveTo(1) },
		"Move":                   func() error { return o.Move(1, 1) },
		"StopMove":               o.StopMove,
		"SyncPosition":           func() error { return o.SyncPosition(1) },
		"SetZeroPosition":        o.SetZeroPosition,
		"ClearStall":             o.ClearStall,
		"SetSpeed":               func() error { return o.SetSpeed(1) },
		"SetHeatingOn":           func() error { return o.SetHeatingOn(true) },
		"FactoryReset":           o.FactoryReset,
		"SetBluetoothName":       func() error { return o.SetBluetoothName("x") },
		"SetFriendlyName":        func() error { return o.SetFriendlyName("x") },
	}
	for name, c := range checks {
		if err := c(); err == nil {
			t.Errorf("%s: got nil err, want an error from the removed device", name)
		}
	}
}

// TestShortReplies: a too-short reply (echo only, no data) must trip each decoder's
// length guard rather than read past the buffer.
func TestShortReplies(t *testing.T) {
	f := newFake()
	for _, op := range []byte{opStatus, opConfig, opConfigExt, opSerial} {
		f.replies[op] = []byte{op} // 1 byte: opcode echo, no length/data
	}
	o := testFoc(f)
	checks := map[string]func() error{
		"Position":               func() error { _, e := o.Position(); return e },
		"Moving":                 func() error { _, e := o.Moving(); return e },
		"TemperatureExternal":    func() error { _, e := o.TemperatureExternal(); return e },
		"TemperatureInternalRaw": func() error { _, e := o.TemperatureInternalRaw(); return e },
		"Config":                 func() error { _, e := o.Config(); return e },
		"ExtConfig":              func() error { _, e := o.ExtConfig(); return e },
		"SerialRaw":              func() error { _, e := o.SerialRaw(); return e },
	}
	for name, c := range checks {
		if err := c(); err == nil {
			t.Errorf("%s: got nil err, want a short-reply error", name)
		}
	}
}
