# oasis-astro

Go drivers for Astroasis Oasis USB accessories, without a vendor SDK:

- `oasisfw`: filter wheel, slot names, focus offsets, and configuration.
- `oasisfoc`: absolute focuser, temperature, motion, and configuration.

## Build and run

Requires Go 1.21 or later. macOS uses IOKit through cgo and requires Apple's
command-line tools. Linux and Windows transports do not require cgo.

```sh
go build -o oasisfwprobe ./cmd/oasisfwprobe
go build -o oasisfocprobe ./cmd/oasisfocprobe
./oasisfwprobe
./oasisfocprobe
```

Each probe opens the first matching device and reads its status. Motion and
configuration flags operate the hardware:

```sh
./oasisfwprobe -goto 2
./oasisfocprobe -moveto 12000
./oasisfocprobe -stop
```

Use `-help` for all options. Configuration tests temporarily change settings
and attempt to restore them; the focuser's heater test also enables heating.

## Use the library

```go
package main

import (
    "fmt"
    "log"

    "github.com/mikefsq/oasis-astro/oasisfw"
)

func run() error {
    device, err := oasisfw.OpenFirst()
    if err != nil {
        return err
    }
    defer device.Close()

    value, err := device.Position()
    if err != nil {
        return err
    }
    fmt.Println(value)
    return nil
}

func main() {
    if err := run(); err != nil {
        log.Fatal(err)
    }
}
```

Both packages provide `Enumerate`, `OpenFirst`, and `OpenAt` for selecting
devices. Filter-wheel slots are zero-based. The focuser provides absolute and
relative moves, position synchronization, and temperature readings.

On Linux, grant the user access to the device's `/dev/hidraw*` node.
For a logged-in desktop user, a udev rule can use:

```text
KERNEL=="hidraw*", ATTRS{idVendor}=="338f", MODE="0660", TAG+="uaccess"
```

A background service needs permissions for its service account, such as a
device group assigned by a local udev rule.

## Development

```sh
go test -race ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
```

Tests use fake transports. See [PROTOCOL.md](PROTOCOL.md) for HID framing
and the package `Transport` interfaces for adding a backend.

## License

[MIT](LICENSE).
