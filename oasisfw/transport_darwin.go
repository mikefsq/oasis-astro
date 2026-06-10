//go:build darwin

package oasisfw

/*
#cgo darwin LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/hid/IOHIDManager.h>
#include <IOKit/hid/IOHIDDevice.h>
#include <CoreFoundation/CoreFoundation.h>
#include <pthread.h>
#include <time.h>
#include <stdlib.h>
#include <string.h>

// Oasis interrupt-HID backend for macOS. Writes use IOHIDDeviceSetReport(Output);
// reads come asynchronously via an input-report callback pumped by a dedicated
// run-loop thread (the standard hidapi-on-macOS pattern), queued for oasis_read to
// pop with a timeout.

#define OASIS_RPT 64   // payload bytes per report (the 0x00 report-ID byte is separate)
#define OASIS_QN  16   // queued input reports

typedef struct {
    IOHIDDeviceRef dev;
    uint8_t  inbuf[OASIS_RPT];          // IOKit fills this on each input report
    uint8_t  q[OASIS_QN][OASIS_RPT];    // ring of received reports
    int      qlen[OASIS_QN];
    int      head, tail, count;
    pthread_mutex_t mu;
    pthread_cond_t  cond;
    pthread_t       thread;
    int      stop;
    int      started;
} oasis_dev;

static void oasis_input_cb(void* ctx, IOReturn r, void* sender, IOHIDReportType type,
                           uint32_t reportID, uint8_t* report, CFIndex len) {
    oasis_dev* d = (oasis_dev*)ctx;
    pthread_mutex_lock(&d->mu);
    if (d->count < OASIS_QN) {
        int n = (len > OASIS_RPT) ? OASIS_RPT : (int)len;
        memcpy(d->q[d->tail], report, n);
        d->qlen[d->tail] = n;
        d->tail = (d->tail + 1) % OASIS_QN;
        d->count++;
        pthread_cond_signal(&d->cond);
    }
    pthread_mutex_unlock(&d->mu);
}

static void* oasis_run(void* arg) {
    oasis_dev* d = (oasis_dev*)arg;
    CFRunLoopRef rl = CFRunLoopGetCurrent();
    IOHIDDeviceScheduleWithRunLoop(d->dev, rl, kCFRunLoopDefaultMode);
    IOHIDDeviceRegisterInputReportCallback(d->dev, d->inbuf, OASIS_RPT, oasis_input_cb, d);
    while (!d->stop) {
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.2, true);
    }
    IOHIDDeviceRegisterInputReportCallback(d->dev, d->inbuf, OASIS_RPT, NULL, NULL);
    IOHIDDeviceUnscheduleFromRunLoop(d->dev, rl, kCFRunLoopDefaultMode);
    return NULL;
}

// oasis_open finds a HID device with VID/PID (and locationID if wantLoc != 0),
// opens it, starts the reader thread, and returns the context (or NULL).
static oasis_dev* oasis_open(uint32_t vid, uint32_t pid, uint32_t wantLoc, uint32_t* outLoc) {
    IOHIDManagerRef mgr = IOHIDManagerCreate(kCFAllocatorDefault, kIOHIDOptionsTypeNone);
    if (!mgr) return NULL;
    IOHIDManagerSetDeviceMatching(mgr, NULL);
    IOHIDManagerOpen(mgr, kIOHIDOptionsTypeNone);
    CFSetRef devs = IOHIDManagerCopyDevices(mgr);
    if (!devs) { CFRelease(mgr); return NULL; }
    CFIndex n = CFSetGetCount(devs);
    IOHIDDeviceRef chosen = NULL; uint32_t chosenLoc = 0;
    if (n > 0) {
        const void** arr = (const void**)malloc(sizeof(void*) * n);
        CFSetGetValues(devs, arr);
        for (CFIndex i = 0; i < n; i++) {
            IOHIDDeviceRef dd = (IOHIDDeviceRef)arr[i];
            int dvid = 0, dpid = 0; uint32_t dloc = 0;
            CFNumberRef v = (CFNumberRef)IOHIDDeviceGetProperty(dd, CFSTR(kIOHIDVendorIDKey));
            CFNumberRef p = (CFNumberRef)IOHIDDeviceGetProperty(dd, CFSTR(kIOHIDProductIDKey));
            CFNumberRef l = (CFNumberRef)IOHIDDeviceGetProperty(dd, CFSTR(kIOHIDLocationIDKey));
            if (v) CFNumberGetValue(v, kCFNumberSInt32Type, &dvid);
            if (p) CFNumberGetValue(p, kCFNumberSInt32Type, &dpid);
            if (l) CFNumberGetValue(l, kCFNumberSInt32Type, &dloc);
            if ((uint32_t)dvid != vid || (uint32_t)dpid != pid) continue;
            if (wantLoc != 0 && dloc != wantLoc) continue;
            chosen = dd; chosenLoc = dloc; break;
        }
        free(arr);
    }
    if (!chosen) { CFRelease(devs); CFRelease(mgr); return NULL; }
    CFRetain(chosen);
    CFRelease(devs);
    if (IOHIDDeviceOpen(chosen, kIOHIDOptionsTypeNone) != kIOReturnSuccess) {
        CFRelease(chosen); CFRelease(mgr); return NULL;
    }
    oasis_dev* d = (oasis_dev*)calloc(1, sizeof(oasis_dev));
    d->dev = chosen;
    pthread_mutex_init(&d->mu, NULL);
    pthread_cond_init(&d->cond, NULL);
    if (pthread_create(&d->thread, NULL, oasis_run, d) == 0) d->started = 1;
    if (outLoc) *outLoc = chosenLoc;
    // mgr intentionally retained so its matching stays alive.
    return d;
}

// oasis_enum lists matching devices' locationIDs into outLoc (up to maxN). Returns count.
static int oasis_enum(uint32_t vid, uint32_t pid, uint32_t* outLoc, int maxN) {
    IOHIDManagerRef mgr = IOHIDManagerCreate(kCFAllocatorDefault, kIOHIDOptionsTypeNone);
    if (!mgr) return -1;
    IOHIDManagerSetDeviceMatching(mgr, NULL);
    IOHIDManagerOpen(mgr, kIOHIDOptionsTypeNone);
    CFSetRef devs = IOHIDManagerCopyDevices(mgr);
    if (!devs) { CFRelease(mgr); return -1; }
    CFIndex n = CFSetGetCount(devs);
    int count = 0;
    if (n > 0) {
        const void** arr = (const void**)malloc(sizeof(void*) * n);
        CFSetGetValues(devs, arr);
        for (CFIndex i = 0; i < n && count < maxN; i++) {
            IOHIDDeviceRef dd = (IOHIDDeviceRef)arr[i];
            int dvid = 0, dpid = 0; uint32_t dloc = 0;
            CFNumberRef v = (CFNumberRef)IOHIDDeviceGetProperty(dd, CFSTR(kIOHIDVendorIDKey));
            CFNumberRef p = (CFNumberRef)IOHIDDeviceGetProperty(dd, CFSTR(kIOHIDProductIDKey));
            CFNumberRef l = (CFNumberRef)IOHIDDeviceGetProperty(dd, CFSTR(kIOHIDLocationIDKey));
            if (v) CFNumberGetValue(v, kCFNumberSInt32Type, &dvid);
            if (p) CFNumberGetValue(p, kCFNumberSInt32Type, &dpid);
            if (l) CFNumberGetValue(l, kCFNumberSInt32Type, &dloc);
            if ((uint32_t)dvid != vid || (uint32_t)dpid != pid) continue;
            outLoc[count++] = dloc;
        }
        free(arr);
    }
    CFRelease(devs); CFRelease(mgr);
    return count;
}

// oasis_write sends one interrupt OUT report. buf[0] is the report ID (0x00); the
// remaining len-1 bytes are the report payload (mirrors hidapi hid_write).
static int oasis_write(oasis_dev* d, uint8_t* buf, int len) {
    if (len < 1) return -1;
    IOReturn r = IOHIDDeviceSetReport(d->dev, kIOHIDReportTypeOutput,
                                      (CFIndex)buf[0], buf + 1, (CFIndex)(len - 1));
    return (r == kIOReturnSuccess) ? len : -1;
}

// oasis_read pops one queued input report into buf, waiting up to timeoutMS
// (<=0 = non-blocking). Returns bytes copied, or 0 if none/timed out.
static int oasis_read(oasis_dev* d, uint8_t* buf, int cap, int timeoutMS) {
    pthread_mutex_lock(&d->mu);
    if (d->count == 0 && timeoutMS > 0) {
        struct timespec ts; clock_gettime(CLOCK_REALTIME, &ts);
        ts.tv_sec  += timeoutMS / 1000;
        ts.tv_nsec += (long)(timeoutMS % 1000) * 1000000L;
        if (ts.tv_nsec >= 1000000000L) { ts.tv_sec++; ts.tv_nsec -= 1000000000L; }
        while (d->count == 0) {
            if (pthread_cond_timedwait(&d->cond, &d->mu, &ts) != 0) break;
        }
    }
    int n = 0;
    if (d->count > 0) {
        n = d->qlen[d->head]; if (n > cap) n = cap;
        memcpy(buf, d->q[d->head], n);
        d->head = (d->head + 1) % OASIS_QN;
        d->count--;
    }
    pthread_mutex_unlock(&d->mu);
    return n;
}

static void oasis_close(oasis_dev* d) {
    if (!d) return;
    if (d->started) {
        d->stop = 1;
        pthread_join(d->thread, NULL);
    }
    IOHIDDeviceClose(d->dev, kIOHIDOptionsTypeNone);
    CFRelease(d->dev);
    pthread_mutex_destroy(&d->mu);
    pthread_cond_destroy(&d->cond);
    free(d);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

type iokitTransport struct{ d *C.oasis_dev }

// Enumerate lists all attached Oasis HID devices by USB locationID.
func Enumerate() ([]DeviceInfo, error) {
	const maxN = 16
	var locs [maxN]C.uint32_t
	n := int(C.oasis_enum(C.uint32_t(VID), C.uint32_t(PID), &locs[0], maxN))
	if n < 0 {
		return nil, errors.New("HID enumeration failed")
	}
	out := make([]DeviceInfo, n)
	for i := 0; i < n; i++ {
		out[i] = DeviceInfo{VID: VID, PID: PID, LocationID: uint32(locs[i])}
	}
	return out, nil
}

func openDev(wantLoc uint32) (Transport, DeviceInfo, error) {
	var loc C.uint32_t
	d := C.oasis_open(C.uint32_t(VID), C.uint32_t(PID), C.uint32_t(wantLoc), &loc)
	if d == nil {
		return nil, DeviceInfo{}, errors.New("no Oasis wheel (none attached, held by another process, or HID access denied)")
	}
	info := DeviceInfo{VID: VID, PID: PID, LocationID: uint32(loc)}
	return &iokitTransport{d: d}, info, nil
}

func openFirst() (Transport, DeviceInfo, error) { return openDev(0) }

// OpenLocation opens the Oasis wheel at a specific USB locationID (from Enumerate).
func OpenLocation(loc uint32) (Transport, DeviceInfo, error) { return openDev(loc) }

func (t *iokitTransport) Write(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, errors.New("empty buffer")
	}
	n := int(C.oasis_write(t.d, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
	if n < 0 {
		return 0, fmt.Errorf("IOHIDDeviceSetReport(output) failed")
	}
	return n, nil
}

func (t *iokitTransport) Read(buf []byte, timeoutMS int) (int, error) {
	if len(buf) == 0 {
		return 0, errors.New("empty buffer")
	}
	n := int(C.oasis_read(t.d, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf)), C.int(timeoutMS)))
	return n, nil
}

func (t *iokitTransport) Close() error {
	if t.d != nil {
		C.oasis_close(t.d)
		t.d = nil
	}
	return nil
}
