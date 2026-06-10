package oasisfw

import (
	"bytes"
	"sync"
	"testing"
)

// fakeHID is an in-memory Transport: it records each command frame (to assert
// encoding) and returns a canned reply keyed by opcode (to assert parsing) — so
// the whole protocol layer is testable with no hardware and no cgo.
type fakeHID struct {
	mu      sync.Mutex
	written [][]byte        // every Write payload
	replies map[byte][]byte // opcode -> reply payload ([0]=opcode echo, [1]=len, [2:]=data)
	drained int             // how many drain (timeout<=0) reads were issued
	failW   bool            // simulate a removed device on Write
	closed  bool
}

func newFake() *fakeHID { return &fakeHID{replies: map[byte][]byte{}} }

func (f *fakeHID) Write(buf []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failW {
		return 0, errDeviceGone
	}
	f.written = append(f.written, append([]byte(nil), buf...))
	return len(buf), nil
}

func (f *fakeHID) Read(buf []byte, timeoutMS int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if timeoutMS <= 0 { // a drain poll: nothing queued
		f.drained++
		return 0, nil
	}
	if len(f.written) == 0 {
		return 0, nil
	}
	op := f.written[len(f.written)-1][1] // frame[1] = opcode
	r, ok := f.replies[op]
	if !ok {
		return 0, nil
	}
	return copy(buf, r), nil
}

func (f *fakeHID) Close() error { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return nil }

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

var errDeviceGone = &deviceGoneErr{}

type deviceGoneErr struct{}

func (*deviceGoneErr) Error() string { return "device removed" }

func testOasis(f *fakeHID) *Oasis { return New(f, DeviceInfo{VID: VID, PID: PID}) }

// tableReply builds an 8-entry int32 big-endian page-table reply (focus/color),
// with entry[slot]=val, data starting at offset tableDataOff (3).
func tableReply(op byte, slot int, val uint32) []byte {
	r := make([]byte, tableDataOff+tableEntries*4)
	r[0], r[1] = op, 0x21
	r[tableDataOff+slot*4+0] = byte(val >> 24)
	r[tableDataOff+slot*4+1] = byte(val >> 16)
	r[tableDataOff+slot*4+2] = byte(val >> 8)
	r[tableDataOff+slot*4+3] = byte(val)
	return r
}

// allReplies gives every opcode a minimal valid reply ([0]=opcode echo) so any
// command completes the transceiver round-trip in encode tests.
func allReplies() map[byte][]byte {
	return map[byte][]byte{
		opInfoA:            {opInfoA, 0x02, 1, 2},
		opInfoB:            {opInfoB, 0x02, 3, 4},
		opSerial:           {opSerial, 0x04, 1, 2, 3, 4},
		opGetFriendlyName:  {opGetFriendlyName, 0x03, 'H', 'i', 0},
		opSetFriendlyName:  {opSetFriendlyName, 0x00},
		opGetBluetoothName: {opGetBluetoothName, 0x03, 'B', 'T', 0},
		opSetBluetoothName: {opSetBluetoothName, 0x00},
		opConfig:           {opConfig, 0x1f, 0, 0, 0, 5, 1, 0, 1, 0}, // mask=5, speed=1, autorun=0, bt=1, turbo=0
		opSetConfig:        {opSetConfig, 0x00},
		opStatus:           {opStatus, 0x08, 0, 0, 0, 0, stateIdle, 0x01, 0, 0}, // filterStatus@6, filterPosition@7
		opFactoryReset:     {opFactoryReset, 0x00},
		opSlotNum:          {opSlotNum, 0x01, 0x07},
		opGetSlotName:      {opGetSlotName, 0x02, 'R', 0},
		opSetSlotName:      {opSetSlotName, 0x00},
		opGetFocusOffset:   tableReply(opGetFocusOffset, 0, 100),
		opSetFocusOffset:   {opSetFocusOffset, 0x00},
		opGetColor:         tableReply(opGetColor, 0, 0x00112233),
		opSetColor:         {opSetColor, 0x00},
		opSetPosition:      {opSetPosition, 0x00},
		opCalibrate:        {opCalibrate, 0x00},
	}
}

// --- Encode: the exact command frame for each operation ---

func TestEncodeCommands(t *testing.T) {
	cases := []struct {
		name string
		do   func(*Oasis)
		want []byte // expected frame prefix: [reportID, opcode, len, payload...]
	}{
		{"status", func(o *Oasis) { o.Status() }, []byte{0x00, opStatus, 0x00}},
		{"set position 3 (1-based on wire)", func(o *Oasis) { o.SetPosition(3) }, []byte{0x00, opSetPosition, 0x01, 0x04}},
		{"slot count", func(o *Oasis) { o.Slots() }, []byte{0x00, opSlotNum, 0x00}},
		{"calibrate", func(o *Oasis) { o.Calibrate() }, []byte{0x00, opCalibrate, 0x01, 0x00}},
		{"serial", func(o *Oasis) { o.SerialRaw() }, []byte{0x00, opSerial, 0x00}},
		{"config", func(o *Oasis) { o.ConfigRaw() }, []byte{0x00, opConfig, 0x00}},
		{"factory reset", func(o *Oasis) { o.FactoryReset() }, []byte{0x00, opFactoryReset, 0x00}},
		{"friendly name get", func(o *Oasis) { o.FriendlyName() }, []byte{0x00, opGetFriendlyName, 0x00}},
		{"slot name get", func(o *Oasis) { o.SlotName(2) }, []byte{0x00, opGetSlotName, 0x01, 0x02}},
		{"slot name set", func(o *Oasis) { o.SetSlotName(1, "R") }, []byte{0x00, opSetSlotName, 0x02, 0x01, 'R'}},
		{"focus offset get", func(o *Oasis) { o.FocusOffset(3) }, []byte{0x00, opGetFocusOffset, 0x21, 0x00}},
		{"focus offset set", func(o *Oasis) { o.SetFocusOffset(3, 100) }, []byte{0x00, opSetFocusOffset, 0x21, 0x00}},
		{"color get", func(o *Oasis) { o.Color(0) }, []byte{0x00, opGetColor, 0x21, 0x00}},
		{"color set", func(o *Oasis) { o.SetColor(0, 0x00112233) }, []byte{0x00, opSetColor, 0x21, 0x00}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			f.replies = allReplies()
			c.do(testOasis(f))
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

// --- The transceiver drains stale input before writing ---

func TestCommandDrainsBeforeWrite(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = []byte{opStatus, 0x06, 0, 0, 0, 0, 0x02, stateIdle, 0, 0}
	testOasis(f).Status()
	if f.drained == 0 {
		t.Error("expected at least one drain read before the command write")
	}
}

// --- Decode: status reply parses to position/state ---

func TestDecodePosition(t *testing.T) {
	f := newFake()
	// reply: [op, len, temp×4, filterStatus=idle, filterPosition=3(1-based), counter…]
	f.replies[opStatus] = []byte{opStatus, 0x08, 0, 0, 0, 0, stateIdle, 0x03, 0, 0}
	pos, err := testOasis(f).Position()
	if err != nil || pos != 2 { // 1-based 3 -> 0-based 2
		t.Fatalf("Position()=%d,%v want 2,nil", pos, err)
	}
}

func TestDecodePositionMoving(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = []byte{opStatus, 0x08, 0, 0, 0, 0, stateMoving, 0x03, 0, 0}
	pos, err := testOasis(f).Position()
	if err != nil || pos != -1 {
		t.Fatalf("Position()=%d,%v want -1,nil (moving)", pos, err)
	}
}

func TestDecodeSlots(t *testing.T) {
	f := newFake()
	f.replies[opSlotNum] = []byte{opSlotNum, 0x01, 0x07}
	n, err := testOasis(f).Slots()
	if err != nil || n != 7 {
		t.Fatalf("Slots()=%d,%v want 7,nil", n, err)
	}
}

func TestDecodeNamesAndFocus(t *testing.T) {
	f := newFake()
	f.replies = allReplies()
	o := testOasis(f)

	if s, err := o.FriendlyName(); err != nil || s != "Hi" {
		t.Fatalf("FriendlyName()=%q,%v want Hi", s, err)
	}
	if s, err := o.SlotName(0); err != nil || s != "R" {
		t.Fatalf("SlotName()=%q,%v want R", s, err)
	}
	if off, err := o.FocusOffset(0); err != nil || off != 100 {
		t.Fatalf("FocusOffset()=%d,%v want 100", off, err)
	}
	if c, err := o.Color(0); err != nil || c != 0x00112233 {
		t.Fatalf("Color()=%#x,%v want 0x112233", c, err)
	}
}

func TestDecodeStateAndConfig(t *testing.T) {
	f := newFake()
	f.replies = allReplies()
	f.replies[opStatus] = []byte{opStatus, 0x08, 0, 0, 0, 0, stateCalibrating, 0x02, 0, 0}
	o := testOasis(f)

	if s, err := o.State(); err != nil || s != stateCalibrating {
		t.Fatalf("State()=%d,%v want %d", s, err, stateCalibrating)
	}
	// busy (calibrating) -> Position -1
	if p, _ := o.Position(); p != -1 {
		t.Fatalf("Position()=%d want -1 while calibrating", p)
	}
	c, err := o.Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.Mask != 5 || c.Speed != 1 || c.Autorun != 0 || c.BluetoothOn != 1 || c.Turbo != 0 {
		t.Fatalf("Config()=%+v want {5 1 0 1 0}", c)
	}
}

func TestBatchAccessors(t *testing.T) {
	f := newFake()
	f.replies = allReplies() // slotNum=7; focus slot0=100; color slot0=0x112233; slot name "R"
	o := testOasis(f)

	fo, err := o.FocusOffsets()
	if err != nil || len(fo) != 7 || fo[0] != 100 {
		t.Fatalf("FocusOffsets()=%v,%v want len 7, [0]=100", fo, err)
	}
	cs, err := o.Colors()
	if err != nil || len(cs) != 7 || cs[0] != 0x00112233 {
		t.Fatalf("Colors()=%v,%v want len 7, [0]=0x112233", cs, err)
	}
	ns, err := o.Names()
	if err != nil || len(ns) != 7 || ns[0] != "R" {
		t.Fatalf("Names()=%v,%v want len 7, [0]=R", ns, err)
	}
}

func TestHandshakeCachesInfo(t *testing.T) {
	f := newFake()
	f.replies = allReplies()
	o := testOasis(f)
	if err := o.Handshake(); err != nil {
		t.Fatal(err)
	}
	if v := o.VersionRaw(); len(v) == 0 {
		t.Error("VersionRaw empty after Handshake")
	}
	if m := o.ModelRaw(); len(m) == 0 {
		t.Error("ModelRaw empty after Handshake")
	}
}

// --- An opcode mismatch in the reply is rejected ---

func TestReplyOpcodeMismatch(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = []byte{0xFF, 0x00} // wrong echo
	if _, err := testOasis(f).Status(); err == nil {
		t.Error("expected error on reply opcode mismatch")
	}
}

// --- A transport error surfaces, never panics ---

func TestDeviceRemoved(t *testing.T) {
	f := newFake()
	f.failW = true
	if _, err := testOasis(f).Position(); err == nil {
		t.Error("Position: want error on removed device")
	}
}

// --- Concurrency: the per-device lock holds under -race ---

func TestConcurrentAccess(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = []byte{opStatus, 0x08, 0, 0, 0, 0, 0x01, stateIdle, 0, 0}
	f.replies[opSlotNum] = []byte{opSlotNum, 0x01, 0x07}
	o := testOasis(f)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				o.Position()
			case 1:
				o.SetPosition(i % 7)
			case 2:
				o.Slots()
			}
		}(i)
	}
	wg.Wait()
}

// configReply is a valid part-1 config block: reply[1] is the true data length (8),
// so patchConfig's read-modify-write bounds it correctly. Data block:
// mask=5 (4B), speed=1, autorun=0, bluetoothOn=1, turbo=0.
func configReply() []byte { return []byte{opConfig, 0x08, 0, 0, 0, 5, 1, 0, 1, 0} }

// TestConfigSetters covers the config write surface (patchConfig + each typed setter):
// each setter reads the block, flips only its byte, and writes opSetConfig with the
// rest preserved. Byte offsets in the data block: speed@4, autorun@5, bluetoothOn@6,
// turbo@7.
func TestConfigSetters(t *testing.T) {
	cases := []struct {
		name string
		do   func(*Oasis) error
		idx  int
		want byte
	}{
		{"speed", func(o *Oasis) error { return o.SetSpeed(9) }, 4, 9},               // 1 -> 9
		{"autorun", func(o *Oasis) error { return o.SetAutorun(true) }, 5, 1},        // 0 -> 1
		{"bluetooth", func(o *Oasis) error { return o.SetBluetoothOn(false) }, 6, 0}, // 1 -> 0
		{"turbo", func(o *Oasis) error { return o.SetTurbo(true) }, 7, 1},            // 0 -> 1
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			f.replies[opConfig] = configReply()
			f.replies[opSetConfig] = []byte{opSetConfig, 0x00}
			if err := c.do(testOasis(f)); err != nil {
				t.Fatal(err)
			}
			w := f.writeFor(opSetConfig)
			if w == nil {
				t.Fatal("no SetConfig command written")
			}
			raw := w[3:] // payload after [reportID, opcode, len]
			if raw[c.idx] != c.want {
				t.Errorf("byte[%d] = %d, want %d", c.idx, raw[c.idx], c.want)
			}
			if !bytes.Equal(raw[0:4], []byte{0, 0, 0, 5}) { // mask preserved
				t.Errorf("mask = % x, want 00 00 00 05 (preserved)", raw[0:4])
			}
		})
	}
}

func TestSetConfigRaw(t *testing.T) {
	f := newFake()
	f.replies[opSetConfig] = []byte{opSetConfig, 0x00}
	block := []byte{0, 0, 0, 9, 2, 1, 0, 1}
	if err := testOasis(f).SetConfigRaw(block); err != nil {
		t.Fatal(err)
	}
	w := f.writeFor(opSetConfig)
	if w == nil || !bytes.Equal(w[3:3+len(block)], block) {
		t.Errorf("SetConfigRaw payload = % x, want % x", w, block)
	}
}

// TestDecodeTemperature covers TemperatureRaw and the Beta-curve conversion: an ADC at
// mid-scale (where (4095-adc)/adc ≈ 1) maps to ≈25 °C.
func TestDecodeTemperature(t *testing.T) {
	f := newFake()
	// status: temp int32 @2:6 = 2048 (0x00000800); state idle; pos 1.
	f.replies[opStatus] = []byte{opStatus, 0x08, 0x00, 0x00, 0x08, 0x00, stateIdle, 0x01, 0, 0}
	o := testOasis(f)
	if raw, err := o.TemperatureRaw(); err != nil || raw != 2048 {
		t.Fatalf("TemperatureRaw()=%d,%v want 2048", raw, err)
	}
	if c, err := o.Temperature(); err != nil || c < 24.5 || c > 25.5 {
		t.Fatalf("Temperature()=%.3f,%v want ≈25.0", c, err)
	}
}

// TestDecodeVersionIdentity covers the version/model/serial parsing (versionField,
// ver4, cstr) over the cached handshake replies.
func TestDecodeVersionIdentity(t *testing.T) {
	f := newFake()
	ver := []byte{opInfoA, 0x18,
		1, 0, 0, 0, // protocol 1.0.0.0
		2, 4, 0, 0, // hardware 2.4.0.0
		1, 7, 1, 0} // firmware 1.7.1.0
	ver = append(ver, append([]byte("Apr 23 2026"), 0)...)
	f.replies[opInfoA] = ver
	f.replies[opInfoB] = append([]byte{opInfoB, 0x11}, append([]byte("OasisFilterWheel"), 0)...)
	f.replies[opSerial] = []byte{opSerial, 0x04, 'S', 'N', '1', 0}

	o := testOasis(f)
	if err := o.Handshake(); err != nil {
		t.Fatal(err)
	}
	if got := o.ProtocolVersion(); got != "1.0.0.0" {
		t.Errorf("ProtocolVersion()=%q, want 1.0.0.0", got)
	}
	if got := o.HardwareVersion(); got != "2.4.0.0" {
		t.Errorf("HardwareVersion()=%q, want 2.4.0.0", got)
	}
	if got := o.FirmwareVersion(); got != "1.7.1.0" {
		t.Errorf("FirmwareVersion()=%q, want 1.7.1.0", got)
	}
	if got := o.FirmwareBuildDate(); got != "Apr 23 2026" {
		t.Errorf("FirmwareBuildDate()=%q, want \"Apr 23 2026\"", got)
	}
	if got := o.Model(); got != "OasisFilterWheel" {
		t.Errorf("Model()=%q, want OasisFilterWheel", got)
	}
	if got, err := o.Serial(); err != nil || got != "SN1" {
		t.Errorf("Serial()=%q,%v want SN1", got, err)
	}
}

// TestRawCommandAndLifecycle covers the Command passthrough plus Info/Close.
func TestRawCommandAndLifecycle(t *testing.T) {
	f := newFake()
	f.replies[opStatus] = []byte{opStatus, 0x08, 0, 0, 0, 0, stateIdle, 0x01, 0, 0}
	o := testOasis(f)
	if _, err := o.Command(opStatus, nil); err != nil {
		t.Errorf("Command(status) err = %v", err)
	}
	if o.Info().VID != VID {
		t.Errorf("Info().VID = %#x, want %#x", o.Info().VID, VID)
	}
	if err := o.Close(); err != nil || !f.closed {
		t.Errorf("Close() err=%v closed=%v, want nil,true", err, f.closed)
	}
}

// TestErrorsPropagate: a removed device (every Write fails) must surface an error
// from every accessor and setter, never a zero value or panic.
func TestErrorsPropagate(t *testing.T) {
	f := newFake()
	f.failW = true
	o := testOasis(f)
	checks := map[string]func() error{
		"Position":        func() error { _, e := o.Position(); return e },
		"State":           func() error { _, e := o.State(); return e },
		"TemperatureRaw":  func() error { _, e := o.TemperatureRaw(); return e },
		"Temperature":     func() error { _, e := o.Temperature(); return e },
		"Slots":           func() error { _, e := o.Slots(); return e },
		"Config":          func() error { _, e := o.Config(); return e },
		"ConfigRaw":       func() error { _, e := o.ConfigRaw(); return e },
		"Serial":          func() error { _, e := o.Serial(); return e },
		"FocusOffset":     func() error { _, e := o.FocusOffset(0); return e },
		"Color":           func() error { _, e := o.Color(0); return e },
		"FocusOffsets":    func() error { _, e := o.FocusOffsets(); return e },
		"Colors":          func() error { _, e := o.Colors(); return e },
		"Names":           func() error { _, e := o.Names(); return e },
		"SlotName":        func() error { _, e := o.SlotName(0); return e },
		"FriendlyName":    func() error { _, e := o.FriendlyName(); return e },
		"BluetoothName":   func() error { _, e := o.BluetoothName(); return e },
		"Handshake":       o.Handshake,
		"SetPosition":     func() error { return o.SetPosition(1) },
		"SetSpeed":        func() error { return o.SetSpeed(1) },
		"SetFocusOffset":  func() error { return o.SetFocusOffset(0, 1) },
		"SetColor":        func() error { return o.SetColor(0, 1) },
		"Calibrate":       o.Calibrate,
		"FactoryReset":    o.FactoryReset,
		"SetConfigRaw":    func() error { return o.SetConfigRaw([]byte{1}) },
		"SetFriendlyName": func() error { return o.SetFriendlyName("x") },
		"SetSlotName":     func() error { return o.SetSlotName(0, "x") },
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
	for _, op := range []byte{opStatus, opConfig, opSerial, opSlotNum, opGetFocusOffset, opGetColor} {
		f.replies[op] = []byte{op} // 1 byte: opcode echo, no length/data
	}
	o := testOasis(f)
	checks := map[string]func() error{
		"Position":       func() error { _, e := o.Position(); return e },
		"State":          func() error { _, e := o.State(); return e },
		"TemperatureRaw": func() error { _, e := o.TemperatureRaw(); return e },
		"Slots":          func() error { _, e := o.Slots(); return e },
		"Config":         func() error { _, e := o.Config(); return e },
		"SerialRaw":      func() error { _, e := o.SerialRaw(); return e },
		"FocusOffset":    func() error { _, e := o.FocusOffset(0); return e },
		"Color":          func() error { _, e := o.Color(0); return e },
	}
	for name, c := range checks {
		if err := c(); err == nil {
			t.Errorf("%s: got nil err, want a short-reply error", name)
		}
	}
}
