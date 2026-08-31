# Framebuffer Input Layer (evdev) — Handoff Notes

Status: **not started.** This document is the plan for adding real keyboard
input to the Linux framebuffer backend so that manual refresh (Ctrl+R) and
rotate (Ctrl+T) work the same as in window mode, without sending signals.

## Where we are today

- `display/framebuffer_linux.go` implements the `DisplayWindow` interface
  (defined in `app.go`) by mmap-ing `/dev/fbN` and blitting RGBA images to it.
  Selected with `-output=framebuffer` (config key `output`, plus
  `-fb=/dev/fbN` / `framebuffer_device` for the device path). 16- and 32-bit
  truecolor only. Aspect-fit scaling + letterbox, status text drawn in a
  bottom strip via `basicfont.Face7x13`.
- **Input is signal-based only** (see `Show()` in that file):
  - `SIGHUP` / `SIGUSR1` → refresh callback (Ctrl+R equivalent)
  - `SIGUSR2` → rotate callback (Ctrl+T equivalent)
  - Test: `kill -s USR1 $(pgrep -f trmnl-go)`
- The callbacks set by `app.go` (`SetOnRefresh` / `SetOnRotate`) do
  non-blocking sends into buffered channels (`app.refreshCh`,
  `app.rotateCh`), so it is safe to invoke them from any goroutine —
  exactly like the signal handler does today. **Reuse that; do not do work
  inside the callback path.**

## Goal

In framebuffer mode, make manual refresh (Ctrl+R) and rotate (Ctrl+T)
work from a physical keyboard without sending signals.

## Plan revision: read the TTY directly, not evdev (primary path)

When the program runs **on its own VT** (the systemd deployment in
systemd-framebuffer.md — `TTYPath=/dev/tty7`, getty replaced), we don't
need evdev at all. The console TTY itself is a readable byte stream:
open our `/dev/tty` in near-raw termios mode and read the key bytes the
console driver produces. This is simpler and better-scoped than evdev:

- No new code to parse the evdev protocol; no `/dev/input` permissions;
  no `input` group / udev rule.
- Input is naturally VT-scoped: we only receive keys while *our* VT is
  active — exactly when the image is on screen. (evdev delivers events
  from every VT, which was a testing wart.)
- Only ~100 lines: termios setup + a read loop + a byte switch.

### Mechanics

- **Termios** (via `golang.org/x/sys/unix`, already a dep): take
  `unix.IoctlGetTermios(fd, unix.TCGETS)`, then clear `ICANON` and
  `ECHO` (and `IXON`) but **keep `ISIG`** — that preserves Ctrl+C →
  SIGINT, which is the graceful-shutdown path. Set `VMIN=1, VTIME=0`.
  `fd` is the one we already hold as `consoleTTY`. **Save the original
  termios and restore it in `releaseConsoleTTY()`** (the service stops
  by SIGTERM; a dead raw TTY with a stale getty on it is confusing).
- **Byte codes** (Linux console, near-raw): the console passes 8-bit
  key values straight through — Ctrl+R is `0x12`, Ctrl+T is `0x14`
  (no ESC prefix for plain Ctrl+letter). Alt/function keys start with
  ESC `0x1B`; ignore anything we don't specifically handle.
- **Key repeat is the real gotcha**: a held key auto-repeats bytes
  (there are no press/release distinctions in the byte stream). Debounce
  instead of filtering: ignore a trigger if one fired within ~2s.
- **Structure**: new `display/ttyinput_linux.go` (`//go:build linux`),
  a `startTTYInput()` goroutine launched from `Show()` alongside
  `watchVT()` (same pattern, exits on `<-w.closed`), started only when
  `consoleTTY != nil`. Fires the same callbacks the signal handlers do.
- **Verify at bring-up**: VT switch sequences (Alt+Fn / Ctrl+Alt+Fn)
  are handled by the kernel's VT layer *before* bytes reach a reader,
  so reading the TTY should not eat them — but confirm on hardware that
  Ctrl+Alt+F1 still escapes to the admin console. If it doesn't, that's
  a stop-the-world bug.

**evdev becomes the fallback path only** — for deployments where the
program does not own a VT (e.g. running under a display manager while
the fb is taken over from another session). Everything below remains
valid for that case.

### (Fallback) evdev path

In framebuffer mode, read a physical keyboard via Linux **evdev**
(`/dev/input/eventN`) and fire:

- Ctrl+R (either Ctrl) → `refreshCallback()`
- Ctrl+T (either Ctrl) → `rotateCallback()`

Keep the signal handlers as-is (they are handy for `cron`/`at`-style
automation and for machines where input setup fails).

## evdev primer (what you need)

Each device is a character file. `read(fd, buf, n)` returns a stream of
`struct input_event`, packed, no alignment padding:

```c
struct input_event {
    struct timeval time;   // 16 bytes on 64-bit (tv_sec + tv_usec)
    uint16_t type;
    uint16_t code;
    int32_t  value;
};
```

So each event is 24 bytes on linux/amd64/arm64.

Types and codes we care about (all in `<linux/input.h>`, none of these
helpers are exposed by `golang.org/x/sys/unix`, so define the consts
ourselves):

| Constant | Value | Meaning |
|---|---|---|
| `EV_SYN` | 2 | Sync event — marks the end of a "frame" of events |
| `EV_KEY` | 1 | Key press/release |
| `KEY_ESC` | 1 | (optional: exit, if we ever want it) |
| `KEY_T` | 20 | t |
| `KEY_R` | 44 | r |
| `KEY_LEFTCTRL` | 29 | left Ctrl |
| `KEY_RIGHTCTRL` | 97 | right Ctrl |

`value` for `EV_KEY`: `0` = release, `1` = press, `2` = **key repeat**
(auto-repeat of a held key). **Must ignore value==2** for the R/T keys or a
held Ctrl+R will trigger a refresh storm.

Key state can also be queried without reading the stream (useful to seed
state so a Ctrl that was held *before* we attached doesn't confuse us):

- `EVIOCGBIT = 0x40044515` with an 8×uint32 buffer: which event *types* the
  device emits (bit 1 set ⇒ has `EV_KEY` ⇒ it's a keyboard/mouse). Use this
  for device discovery (skip devices without EV_KEY).
- `EVIOCGKEY = 0x40044518` with an 8×uint32 buffer: which keys are currently
  pressed.
- `ioctl(fd, 0x40044500 /* EVIOCGID */, id)` if we ever want vendor/product
  IDs for nicer logging.

Both ioctls: raw `unix.Syscall(unix.SYS_IOCTL, fd, req, ptr)` with a
`[]byte` buffer (`golang.org/x/sys/unix` does not expose a generic
`Ioctl` in the version pinned here).

**Hard-won caution:** verify every ioctl constant against
`/usr/include/linux/*.h` — do not derive it. The fbdev ioctls turned out
NOT to use `_IOR` size-encoding (they're plain 16-bit values like
`0x4600`; sending `0xA04600` gets `ENOTTY`), even though C tooling will
happily let you recompute "the" encoding from `sizeof`. (The evdev
numbers above are genuinely `_IOWR`-encoded — but check them the same
way before using.)

## Implementation plan

### 1. Config

`config/config.go`:

```go
// InputDevice is the evdev device to read keyboard input from when
// Output == "framebuffer". Empty string means auto-detect the first
// /dev/input/eventN that reports EV_KEY.
InputDevice string `json:"input_device,omitempty"`
```

New flag in `app.go`: `-input` (string), same wiring pattern as `-fb`.
Add to the verbose startup banner when output == framebuffer.

### 2. New file: `display/evdev_linux.go` (`//go:build linux`)

A small, dependency-free reader (~150 lines) using only
`golang.org/x/sys/unix` (already a direct dep after this feature landed —
check `go.mod`):

```go
type evdevInput struct {
    devPath string
    fd      int
    // keydown state for the two Ctrl keys, seeded from EVIOCGKEY
}
```

- `openKeyboard(path)`: open `O_RDONLY`, verify `EVIOCGBIT` has bit 1
  (`EV_KEY`), seed Ctrl state from `EVIOCGKEY`.
- `run(ctx, onRefresh, onRotate func()) error`: loop on
  `unix.Poll`/`unix.Read` over 24-byte `input_event`s. Keep
  `ctrlDown := int` (count of L+R). On `EV_KEY` value==1:
  - KEY_LEFTCTRL/KEY_RIGHTCTRL → `ctrlDown++`
  - KEY_R with `ctrlDown > 0` → `onRefresh()`
  - KEY_T with `ctrlDown > 0` → `onRotate()`
  On value==0: decrement / ignore. Ignore value==2 (repeat).
  Drain events until `EV_SYN`/SYN_REPORT(0) to keep the stream in step
  (strictly not required for correctness here since we track per-key state,
  but it matches the protocol).
- Auto-detect helper: scan `/dev/input/event*` (sorted), `openKeyboard`
  each until one succeeds (has EV_KEY). Log the chosen path when verbose.

Notes:
- Reading evdev requires access to the device (root, or the `input` group,
  or a udev rule:
  `SUBSYSTEM=="input", KERNEL=="event*", MODE="0660", GROUP="input"`).
- One reader goroutine per device; a single auto-detected keyboard is the
  target use case. Don't over-engineer for multiple keyboards — but the
  struct should not hard-code anything that would prevent reading two.
- Errors: if the device disappears (unplug), log and return; the app should
  treat input loss as non-fatal (signals still work).

### 3. Wiring in `display/framebuffer_linux.go`

In `Show()`, alongside the existing signal goroutine:

```go
go func() {
    if err := startInputLoop(w.config, w.verbose, func() { ...refresh... }, func() { ...rotate... }); err != nil {
        if w.verbose { log to stderr: input disabled, signals still work }
    }
}()
```

Factor the callback-firing bodies out of the signal handler into two small
closures/methods (`triggerRefresh()`, `triggerRotate()`) shared by both the
signal loop and the evdev loop. Update the package doc comment (the
"Signalling" paragraph) to mention that keyboard input is now supported and
signals remain as an alternative.

### 4. README + config docs

- Document `-output=framebuffer`, `-fb`, `-input` in README.md (there is a
  flags section; match its style).
- Note the `input`/`video` group or udev rules needed for unprivileged
  operation, and that /dev/fb0 takes over the console (already in the Go
  doc comment).

## Build notes (learned the hard way)

Two build flavors exist:

- **Normal build** (Fyne window mode + framebuffer mode in one binary):
  requires CGO + GL headers; use `./build-all.sh` / `fyne-cross` (needs
  docker/podman). Plain `go build` with `CGO_ENABLED=0` does NOT work
  (Fyne/GLFW pulls in go-gl, which needs cgo).
- **Headless build** (`-tags headless`, Linux-only): excludes Fyne entirely
  (`display/window.go` is tagged `!headless`; `app_linux_headless.go` takes
  over `createWindow`). Pure Go, no cgo, no GL/X11:

  ```sh
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags headless -o trmnl-go-linux-arm64 .
  ```

  The evdev input layer uses only `golang.org/x/sys/unix` (already a direct
  dep), so it works in both flavors. The evdev reader file must be tagged
  `//go:build linux` (like `display/framebuffer_linux.go`), which keeps it
  available in both normal and headless Linux builds.

## Testing

1. **Build**: headless flavor (command above) for quick iteration; the
   normal flavor via `./build-all.sh linux` where docker is available.
2. **On a machine with a console fb** (Pi, or a VM like
   `qemu-system-x86_64 -display none -vga std` where /dev/fb0 exists):
   run with `-output=framebuffer -verbose`, watch the startup screen appear
   on the console/VM display, confirm status strip updates.
3. **Keyboard**: type Ctrl+R / Ctrl+T on a physical keyboard attached to
   the machine; confirm verbose logs show the evdev device chosen and the
   callbacks fire. Hold Ctrl+R to confirm **no repeat storm** (value==2
   filtering).
4. **Signals still work** while evdev is attached: `kill -s USR1 <pid>`.
5. No unit tests exist in this repo for the display layer; manual testing
   on hardware is the bar. (If it's ever wanted: the 24-byte event parsing
   could be tested with a `unix.Socketpair`/`os.Pipe` feeding crafted
   `input_event` bytes — cheap and platform-portable.)

## Interaction with the VT/cursor handling

`display/framebuffer_linux.go` already manages the console TTY:
`acquireConsoleTTY()` holds our `/dev/tty` if it's a VT, `watchVT()`
polls `TIOCGCONS` (500ms ticker) and repaints on the away->active
transition. The input layer is orthogonal:

- **evdev events are NOT VT-scoped.** A keyboard event on
  `/dev/input/eventN` reaches every process with the device open,
  regardless of which VT is active. So Ctrl+R from the keyboard will
  trigger a refresh even while the user is sitting on a different VT.
  That's acceptable (the actions are idempotent and the refresh just
  re-fetches), but be aware of it in testing: the verbose log will show
  refreshes that don't appear to change the visible console.
- **Do NOT use `EVIOCGRAB`.** Excluding the kernel from the keyboard
  would break Ctrl+Alt+F<n> VT switching, which is the user's only way
  to escape the kiosk and (per `watchVT`) to come back to the image.
  Reading the device ungrabbed shares events with the VT layer — both
  keep working.
- The input loop is just one more goroutine started from `Show()`;
  `watchVT()` is the existing model for a Show()-started goroutine that
  must exit on `<-w.closed`. Follow that pattern.

## Deliberate non-goals (for now)

- Mouse / touch / tablet input.
- Key remapping or per-key bindings beyond Ctrl+R / Ctrl+T.
- Esc-to-exit on the framebuffer (deliberately: an accidental Esc on a
  kiosk should not kill the display; SIGTERM exits cleanly).
- Wayland/X11 input for the Fyne backend (out of scope — Fyne handles it).
