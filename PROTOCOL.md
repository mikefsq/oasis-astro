# Oasis HID protocol

Both devices use interrupt reports with big-endian numeric fields.
Commands include a zero report ID and are padded to 65 bytes:

```text
command: [0]=report ID, [1]=opcode, [2]=payload length, [3..]=payload
reply:   [0]=opcode echo, [1]=data length, [2..]=data
```

| Device | Vendor ID | Product ID |
|---|---|---|
| Filter wheel | 0x338F | 0x0FE0 |
| Focuser | 0x338F | 0xA0F0 |

The transport writes output reports and reads input reports. A read with a
nonpositive timeout polls without waiting, allowing stale input to be drained
before a command. Preserve opcode matching when handling queued replies.

macOS uses IOKit output reports and input callbacks; Linux uses nonblocking
hidraw I/O; Windows uses SetupAPI and overlapped reads/writes. C symbols are
prefixed per package so wheel and focuser backends can link in one binary.

Device methods and opcode constants live in [oasisfw](oasisfw) and
[oasisfoc](oasisfoc). Keep wire-format validation in protocol tests using fake
transports; hardware tests should document any motion or persistent writes.
