// Command oasisfocprobe reads device status and provides diagnostic controls.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mikefsq/oasis-astro/oasisfoc"
)

func main() {
	moveTo := flag.Int("moveto", -1, "absolute move to position, then watch; -1 = read-only")
	move := flag.String("move", "", "relative move 'dir:steps' (e.g. 1:500)")
	moveIn := flag.Int("in", -1, "move IN by N steps (decreasing position), then watch")
	moveOut := flag.Int("out", -1, "move OUT by N steps (increasing position), then watch")
	beep := flag.String("beep", "", "set beep-on-move on|off, then read back")
	beepStart := flag.String("beepstart", "", "set beep-on-startup on|off, then read back")
	setFriendly := flag.String("setfriendly", "", "set the friendly name, then read back")
	clearFriendly := flag.Bool("clearfriendly", false, "clear the friendly name (set empty)")
	heaterTest := flag.Bool("heatertest", false, "briefly enable the heater (target 45°C) for ~30s, watch temp, then restore original heater settings")
	usbPower := flag.Int("usbpower", -1, "set usbPowerCapacity to N, then read back")
	stop := flag.Bool("stop", false, "halt motion")
	sync := flag.Int("sync", -1, "set reported position to N without moving; -1 = skip")
	setZero := flag.Bool("setzero", false, "define current position as zero")
	clearStall := flag.Bool("clearstall", false, "clear a stall condition")
	watch := flag.Bool("watch", false, "poll position+moving repeatedly")
	cmd := flag.String("cmd", "", "issue a single READ opcode (hex, e.g. 0x11) with no payload and dump the raw reply")
	stopTest := flag.Int("stoptest", -1, "MoveTo this far target, let it run ~1s, then StopMove and report the halted position")
	setBT := flag.String("setbt", "", "set the bluetooth name (then read back); pass the current value to validate the write path harmlessly")
	cfgTest := flag.Bool("cfgtest", false, "validate SetConfig: no-op round-trip, then toggle beepOnMove and restore (safe; all changes reverted)")
	extCfgTest := flag.Bool("extcfgtest", false, "validate ext SetConfig: write-back-same, then toggle stallDetection and restore (safe)")
	flag.Parse()

	o, err := oasisfoc.OpenFirst()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer o.Close()

	info := o.Info()
	fmt.Printf("opened Oasis focuser: VID=0x%04x PID=0x%04x loc=0x%x\n", info.VID, info.PID, info.LocationID)

	if *cmd != "" {
		op, err := strconv.ParseUint(strings.TrimPrefix(*cmd, "0x"), 16, 8)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad -cmd %q: %v\n", *cmd, err)
			os.Exit(2)
		}
		r, err := o.Command(byte(op), nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cmd 0x%02x: %v\n", op, err)
			os.Exit(1)
		}
		fmt.Printf("cmd 0x%02x reply (%d bytes): % x\n", op, len(r), r)
		return
	}

	dumpAll(o)

	switch {
	case *stopTest >= 0:
		p0, _ := o.Position()
		fmt.Printf("\nstoptest: at %d, MoveTo %d then stop mid-flight...\n", p0, *stopTest)
		report("MoveTo", o.MoveTo(int32(*stopTest)))
		for i := 0; i < 20; i++ { // wait until it's actually moving
			if m, _ := o.Moving(); m {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(1 * time.Second) // let it travel
		report("StopMove", o.StopMove())
		time.Sleep(300 * time.Millisecond)
		p, _ := o.Position()
		m, _ := o.Moving()
		fmt.Printf("after stop: position=%d moving=%v (target was %d; halted short = StopMove works)\n", p, m, *stopTest)
	case *cfgTest:
		c0, err := o.Config()
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(1)
		}
		fmt.Printf("\nconfig: %+v\n", c0)
		// 1) no-op round-trip: write the unchanged config, confirm nothing changes.
		if err := o.SetConfig(c0); err != nil {
			fmt.Fprintln(os.Stderr, "SetConfig(no-op):", err)
			os.Exit(1)
		}
		c1, _ := o.Config()
		fmt.Printf("after no-op write: identical=%v\n", c1 == c0)
		if c1 != c0 {
			fmt.Fprintln(os.Stderr, "ABORT: no-op write changed the config; not safe to continue")
			os.Exit(1)
		}
		// 2) toggle beepOnMove, verify only it changed, then restore.
		report("SetBeepOnMove(toggle)", o.SetBeepOnMove(c0.BeepOnMove == 0))
		c2, _ := o.Config()
		want := c0
		want.BeepOnMove = 0
		if c0.BeepOnMove == 0 {
			want.BeepOnMove = 1
		}
		fmt.Printf("after toggle: beepOnMove %d->%d, only-that-changed=%v\n", c0.BeepOnMove, c2.BeepOnMove, c2 == want)
		report("SetBeepOnMove(restore)", o.SetBeepOnMove(c0.BeepOnMove != 0))
		c3, _ := o.Config()
		fmt.Printf("after restore: fully identical to start=%v\n", c3 == c0)
	case *extCfgTest:
		e0, err := o.ExtConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "extconfig:", err)
			os.Exit(1)
		}
		fmt.Printf("\nextconfig: %+v\n", e0)
		// write-back same value (no-op), confirm nothing changes
		report("SetStallDetection(same)", o.SetStallDetection(e0.StallDetection != 0))
		e1, _ := o.ExtConfig()
		fmt.Printf("after write-back-same: identical=%v\n", e1 == e0)
		if e1 != e0 {
			fmt.Fprintln(os.Stderr, "ABORT: write-back changed ext config")
			os.Exit(1)
		}
		// toggle stallDetection, verify only it changed, restore
		report("SetStallDetection(toggle)", o.SetStallDetection(e0.StallDetection == 0))
		e2, _ := o.ExtConfig()
		want := e0
		want.StallDetection = 0
		if e0.StallDetection == 0 {
			want.StallDetection = 1
		}
		fmt.Printf("after toggle: stall %d->%d, only-that-changed=%v\n", e0.StallDetection, e2.StallDetection, e2 == want)
		report("SetStallDetection(restore)", o.SetStallDetection(e0.StallDetection != 0))
		e3, _ := o.ExtConfig()
		fmt.Printf("after restore: identical to start=%v\n", e3 == e0)
	case *setBT != "":
		before, _ := o.BluetoothName()
		report("SetBluetoothName", o.SetBluetoothName(*setBT))
		after, _ := o.BluetoothName()
		fmt.Printf("bluetooth name: %q -> %q (wrote %q)\n", before, after, *setBT)
	case *stop:
		report("StopMove", o.StopMove())
	case *setZero:
		report("SetZeroPosition", o.SetZeroPosition())
	case *clearStall:
		report("ClearStall", o.ClearStall())
	case *sync >= 0:
		report("SyncPosition", o.SyncPosition(int32(*sync)))
	case *usbPower >= 0:
		e0, _ := o.ExtConfig()
		report("SetUsbPowerCapacity", o.SetUsbPowerCapacity(int32(*usbPower)))
		e1, _ := o.ExtConfig()
		want := e0
		want.UsbPowerCapacity = int32(*usbPower)
		fmt.Printf("usbPowerCapacity %d -> %d, only-that-changed=%v\n", e0.UsbPowerCapacity, e1.UsbPowerCapacity, e1 == want)
	case *heaterTest:
		e0, err := o.ExtConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "extconfig:", err)
			os.Exit(1)
		}
		fmt.Printf("\noriginal heater: on=%d target=%d(centi°C)\n", e0.HeatingOn, e0.HeatingTemperature)
		report("SetHeatingTemperature(4500=45°C)", o.SetHeatingTemperature(4500))
		report("SetHeatingOn(true)", o.SetHeatingOn(true))
		e1, _ := o.ExtConfig()
		fmt.Printf("heater ON: on=%d target=%d  >>> FEEL THE MOTOR NOW <<<\n", e1.HeatingOn, e1.HeatingTemperature)
		for i := 0; i < 6; i++ {
			time.Sleep(5 * time.Second)
			ti, _ := o.TemperatureInternal()
			fmt.Printf("  t=%2ds  internal=%.2f °C\n", (i+1)*5, ti)
		}
		report("SetHeatingOn(false)", o.SetHeatingOn(false))
		report("SetHeatingTemperature(restore)", o.SetHeatingTemperature(e0.HeatingTemperature))
		e2, _ := o.ExtConfig()
		fmt.Printf("restored: on=%d target=%d (identical to original: %v)\n", e2.HeatingOn, e2.HeatingTemperature, e2 == e0)
	case *clearFriendly:
		report("SetFriendlyName(clear)", o.SetFriendlyName(""))
		after, _ := o.FriendlyName()
		fmt.Printf("friendly name now: %q\n", after)
	case *setFriendly != "":
		before, _ := o.FriendlyName()
		report("SetFriendlyName", o.SetFriendlyName(*setFriendly))
		after, _ := o.FriendlyName()
		fmt.Printf("friendly name: %q -> %q\n", before, after)
	case *beep != "":
		on := *beep == "on" || *beep == "1" || *beep == "true"
		report("SetBeepOnMove", o.SetBeepOnMove(on))
		c, _ := o.Config()
		fmt.Printf("beepOnMove = %d\n", c.BeepOnMove)
	case *beepStart != "":
		on := *beepStart == "on" || *beepStart == "1" || *beepStart == "true"
		report("SetBeepOnStartup", o.SetBeepOnStartup(on))
		c, _ := o.Config()
		fmt.Printf("beepOnStartup = %d\n", c.BeepOnStartup)
	case *moveOut >= 0:
		p0, _ := o.Position()
		fmt.Printf("\nat %d, MoveOut %d (expect +)...\n", p0, *moveOut)
		report("MoveOut", o.MoveOut(int32(*moveOut)))
		watchSettle(o)
	case *moveIn >= 0:
		p0, _ := o.Position()
		fmt.Printf("\nat %d, MoveIn %d (expect -)...\n", p0, *moveIn)
		report("MoveIn", o.MoveIn(int32(*moveIn)))
		watchSettle(o)
	case *move != "":
		dir, steps := mustDirSteps(*move)
		report("Move", o.Move(dir, steps))
		watchSettle(o)
	case *moveTo >= 0:
		fmt.Printf("\nmoving to %d...\n", *moveTo)
		report("MoveTo", o.MoveTo(int32(*moveTo)))
		watchSettle(o)
	case *watch:
		fmt.Println("\nwatching (Ctrl-C to stop)...")
		for {
			p, _ := o.Position()
			m, _ := o.Moving()
			fmt.Printf("position=%d moving=%v\n", p, m)
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func dumpAll(o *oasisfoc.Oasis) {
	fmt.Println("\n-- identity --")
	if s, err := o.Serial(); err == nil {
		fmt.Printf("serial      : %s\n", s)
	}
	fmt.Printf("model       : %s\n", o.Model())
	fmt.Printf("hardware ver: %s\n", o.HardwareVersion())
	fmt.Printf("firmware ver: %s\n", o.FirmwareVersion())
	fmt.Printf("protocol ver: %s\n", o.ProtocolVersion())
	fmt.Printf("fw built    : %s\n", o.FirmwareBuildDate())

	fmt.Println("\n-- status --")
	if r, err := o.Status(); err == nil {
		fmt.Printf("raw         : % x\n", r)
	} else {
		fmt.Fprintln(os.Stderr, "status:", err)
	}
	if p, err := o.Position(); err == nil {
		fmt.Printf("position    : %d\n", p)
	}
	if m, err := o.Moving(); err == nil {
		fmt.Printf("moving      : %v\n", m)
	}
	if te, err := o.TemperatureExternal(); err == nil {
		fmt.Printf("temp (ext)  : %.2f °C\n", te)
	}
	if ti, err := o.TemperatureInternalRaw(); err == nil {
		c, _ := o.TemperatureInternal()
		fmt.Printf("temp (int)  : %.2f °C (raw ADC %d, Beta-curve)\n", c, ti)
	}

	fmt.Println("\n-- config (part 1: 0x30) --")
	if c, err := o.Config(); err == nil {
		fmt.Printf("%+v\n", c)
	} else {
		fmt.Fprintln(os.Stderr, "config:", err)
	}
	fmt.Println("-- ext config (part 2: 0x3a — heating/stall/usbPower) --")
	if e, err := o.ExtConfig(); err == nil {
		fmt.Printf("stall=%d heatingTemp=%d(centi°C) heatingOn=%d usbPower=%d\n",
			e.StallDetection, e.HeatingTemperature, e.HeatingOn, e.UsbPowerCapacity)
	} else {
		fmt.Fprintln(os.Stderr, "extconfig:", err)
	}
}

func report(label string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", label, err)
		return
	}
	fmt.Printf("%s: ok\n", label)
}

func mustDirSteps(s string) (int, int32) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		fmt.Fprintf(os.Stderr, "expected 'dir:steps', got %q\n", s)
		os.Exit(2)
	}
	dir, err1 := strconv.Atoi(parts[0])
	steps, err2 := strconv.ParseInt(parts[1], 10, 32)
	if err1 != nil || err2 != nil {
		fmt.Fprintf(os.Stderr, "bad 'dir:steps' %q\n", s)
		os.Exit(2)
	}
	return dir, int32(steps)
}

func watchSettle(o *oasisfoc.Oasis) {
	for i := 0; i < 120; i++ {
		m, err := o.Moving()
		if err != nil {
			fmt.Fprintln(os.Stderr, "moving:", err)
			return
		}
		if !m {
			p, _ := o.Position()
			fmt.Printf("settled at %d\n", p)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("gave up waiting for the focuser to settle")
}
