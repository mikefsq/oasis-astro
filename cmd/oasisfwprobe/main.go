// Command oasisfwprobe exercises the pure-Go Oasis filter-wheel driver against a
// real wheel: it opens the first wheel, dumps the full decoded read surface
// (identity, status, config, names, focus offsets, colors), can move/calibrate,
// and can drive the (persistent) setters with read-back verification.
//
//	oasisfwprobe                    # read-only: dump everything the driver decodes
//	oasisfwprobe -goto 2            # move to slot 2 (0-based), then watch it settle
//	oasisfwprobe -calibrate         # run the home/realign routine
//	oasisfwprobe -watch             # poll position+state repeatedly
//
//	# PERSISTENT writes (each writes then reads back to verify):
//	oasisfwprobe -setfocus 2:-150   # slot 2 focus offset = -150
//	oasisfwprobe -setcolor 0:00ff00 # slot 0 color = 0x00ff00 (RRGGBB hex)
//	oasisfwprobe -setslotname 1:Ha  # slot 1 name = "Ha"
//	oasisfwprobe -setbtname OasisFW  # bluetooth name
//	oasisfwprobe -setconfig 00000005...  # raw config block (hex)
//	oasisfwprobe -factoryreset      # restore factory defaults

package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mikefsq/oasis-astro/oasisfw"
)

func main() {
	gotoSlot := flag.Int("goto", -1, "move to 0-based slot, then watch it settle; -1 = read-only")
	calibrate := flag.Bool("calibrate", false, "run the home/realign routine, then watch it settle")
	watch := flag.Bool("watch", false, "poll position+state repeatedly")

	setFocus := flag.String("setfocus", "", "PERSISTENT: set focus offset, 'slot:offset' (e.g. 2:-150)")
	setColor := flag.String("setcolor", "", "PERSISTENT: set slot color, 'slot:RRGGBB' hex (e.g. 0:00ff00)")
	setSlotName := flag.String("setslotname", "", "PERSISTENT: set slot name, 'slot:name'")
	setBTName := flag.String("setbtname", "", "PERSISTENT: set bluetooth name")
	setName := flag.String("setname", "", "PERSISTENT: set friendly name")
	clearName := flag.Bool("clearname", false, "PERSISTENT: clear the friendly name (set empty)")
	setConfig := flag.String("setconfig", "", "PERSISTENT: write raw config block (hex bytes)")
	factoryReset := flag.Bool("factoryreset", false, "PERSISTENT: restore factory defaults")
	cfgTest := flag.Bool("cfgtest", false, "validate config read-modify-write: no-op round-trip + toggle turbo + restore")
	flag.Parse()

	o, err := oasisfw.OpenFirst()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer o.Close()

	info := o.Info()
	fmt.Printf("opened Oasis wheel: VID=0x%04x PID=0x%04x loc=0x%x\n", info.VID, info.PID, info.LocationID)
	dumpAll(o)

	// PERSISTENT writes (write, then read back to verify).
	if *setFocus != "" {
		slot, v := mustSlotInt(*setFocus)
		do("SetFocusOffset", o.SetFocusOffset(slot, int32(v)))
		readback("FocusOffset", func() (any, error) { return o.FocusOffset(slot) })
	}
	if *setColor != "" {
		slot, v := mustSlotHex(*setColor)
		do("SetColor", o.SetColor(slot, uint32(v)))
		readback("Color", func() (any, error) { c, e := o.Color(slot); return fmt.Sprintf("%#06x", c), e })
	}
	if *setSlotName != "" {
		slot, name := mustSlotStr(*setSlotName)
		do("SetSlotName", o.SetSlotName(slot, name))
		readback("SlotName", func() (any, error) { return o.SlotName(slot) })
	}
	if *setBTName != "" {
		do("SetBluetoothName", o.SetBluetoothName(*setBTName))
		readback("BluetoothName", func() (any, error) { return o.BluetoothName() })
	}
	if *setName != "" {
		do("SetFriendlyName", o.SetFriendlyName(*setName))
		readback("FriendlyName", func() (any, error) { return o.FriendlyName() })
	}
	if *clearName {
		do("SetFriendlyName(clear)", o.SetFriendlyName(""))
		readback("FriendlyName", func() (any, error) { return o.FriendlyName() })
	}
	if *setConfig != "" {
		b, err := hex.DecodeString(strings.TrimPrefix(*setConfig, "0x"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "setconfig: bad hex:", err)
			os.Exit(2)
		}
		do("SetConfigRaw", o.SetConfigRaw(b))
		readback("Config", func() (any, error) { return o.Config() })
	}
	if *factoryReset {
		do("FactoryReset", o.FactoryReset())
	}
	if *cfgTest {
		c0, _ := o.Config()
		fmt.Printf("\nconfig: %+v\n", c0)
		raw, _ := o.ConfigRaw()
		do("SetConfigRaw(no-op)", o.SetConfigRaw(raw))
		c1, _ := o.Config()
		fmt.Printf("after no-op write: identical=%v\n", c1 == c0)
		do("SetTurbo(toggle)", o.SetTurbo(c0.Turbo == 0))
		c2, _ := o.Config()
		want := c0
		want.Turbo = 0
		if c0.Turbo == 0 {
			want.Turbo = 1
		}
		fmt.Printf("after toggle: turbo %d->%d, only-that-changed=%v\n", c0.Turbo, c2.Turbo, c2 == want)
		do("SetTurbo(restore)", o.SetTurbo(c0.Turbo != 0))
		c3, _ := o.Config()
		fmt.Printf("after restore: identical to start=%v\n", c3 == c0)
	}

	switch {
	case *calibrate:
		fmt.Println("\ncalibrating...")
		if err := o.Calibrate(); err != nil {
			fmt.Fprintln(os.Stderr, "calibrate:", err)
			os.Exit(1)
		}
		watchSettle(o)
	case *gotoSlot >= 0:
		fmt.Printf("\nmoving to slot %d...\n", *gotoSlot)
		if err := o.SetPosition(*gotoSlot); err != nil {
			fmt.Fprintln(os.Stderr, "set position:", err)
			os.Exit(1)
		}
		watchSettle(o)
	case *watch:
		fmt.Println("\nwatching (Ctrl-C to stop)...")
		for {
			pos, _ := o.Position()
			st, _ := o.State()
			fmt.Printf("position=%d state=%d (%s)\n", pos, st, stateName(st))
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// dumpAll prints every read-only field the driver decodes
func dumpAll(o *oasisfw.Oasis) {
	fmt.Println("\n-- identity --")
	showHex("version raw", o.VersionRaw(), nil)
	showHex("model raw", o.ModelRaw(), nil)
	if s, err := o.FriendlyName(); err == nil {
		fmt.Printf("friendly name : %q\n", s)
	}
	if s, err := o.BluetoothName(); err == nil {
		fmt.Printf("bluetooth name: %q\n", s)
	}
	raw, errs := o.SerialRaw()
	showHex("serial raw", raw, errs)

	fmt.Println("\n-- status --")
	if r, err := o.Status(); err == nil {
		fmt.Printf("raw         : % x\n", r)
	} else {
		fmt.Fprintln(os.Stderr, "status:", err)
	}
	if st, err := o.State(); err == nil {
		fmt.Printf("state       : %d (%s)\n", st, stateName(st))
	}
	if p, err := o.Position(); err == nil {
		fmt.Printf("position    : %d (-1 = moving/unknown)\n", p)
	}
	if n, err := o.Slots(); err == nil {
		fmt.Printf("slots       : %d\n", n)
	}
	if c, err := o.Temperature(); err == nil {
		raw, _ := o.TemperatureRaw()
		fmt.Printf("temperature : %.2f °C (raw ADC %d)\n", c, raw)
	}

	fmt.Println("\n-- config --")
	if c, err := o.Config(); err == nil {
		fmt.Printf("%+v\n", c)
	} else {
		fmt.Fprintln(os.Stderr, "config:", err)
	}

	fmt.Println("\n-- per-slot --")
	if ns, err := o.Names(); err == nil {
		fmt.Printf("names         : %q\n", ns)
	}
	if fo, err := o.FocusOffsets(); err == nil {
		fmt.Printf("focus offsets : %v\n", fo)
	}
	if cs, err := o.Colors(); err == nil {
		fmt.Printf("colors        : %#08x\n", cs)
	}
}

// do reports a setter result.
func do(label string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", label, err)
		return
	}
	fmt.Printf("%s: ok\n", label)
}

// readback prints a getter result, for verifying a preceding write.
func readback(label string, get func() (any, error)) {
	v, err := get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s (readback): %v\n", label, err)
		return
	}
	fmt.Printf("%s (readback): %v\n", label, v)
}

func showHex(label string, b []byte, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", label, err)
		return
	}
	fmt.Printf("%-13s : % x\n", label, b)
}

func stateName(s int) string {
	switch s {
	case 0:
		return "idle"
	case 1:
		return "moving"
	case 2:
		return "calibrating"
	case 3:
		return "benchmarking"
	default:
		return "error/unknown"
	}
}

// mustSlotInt parses "slot:int" (decimal, signed).
func mustSlotInt(s string) (int, int64) {
	slot, rest := splitSlot(s)
	v, err := strconv.ParseInt(rest, 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad value %q: %v\n", s, err)
		os.Exit(2)
	}
	return slot, v
}

// mustSlotHex parses "slot:hex".
func mustSlotHex(s string) (int, uint64) {
	slot, rest := splitSlot(s)
	v, err := strconv.ParseUint(strings.TrimPrefix(rest, "0x"), 16, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad hex value %q: %v\n", s, err)
		os.Exit(2)
	}
	return slot, v
}

// mustSlotStr parses "slot:string" (value is the remainder after the first colon).
func mustSlotStr(s string) (int, string) {
	slot, rest := splitSlot(s)
	return slot, rest
}

func splitSlot(s string) (int, string) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		fmt.Fprintf(os.Stderr, "expected 'slot:value', got %q\n", s)
		os.Exit(2)
	}
	slot, err := strconv.Atoi(parts[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad slot %q: %v\n", s, err)
		os.Exit(2)
	}
	return slot, parts[1]
}

func watchSettle(o *oasisfw.Oasis) {
	for i := 0; i < 60; i++ {
		pos, err := o.Position()
		if err != nil {
			fmt.Fprintln(os.Stderr, "position:", err)
			return
		}
		if pos >= 0 {
			fmt.Printf("settled at slot %d\n", pos)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("gave up waiting for the wheel to settle")
}
