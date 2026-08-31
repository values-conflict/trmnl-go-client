# TTY Input — Implementation Notes

Status: **implemented** (see `display/framebuffer_linux.go`:
`watchInput`, the termios block in `acquireConsoleTTY`, and the restore in
`releaseConsoleTTY`). This document replaces the earlier evdev plan, which
was **dropped in favor of reading the controlling TTY directly**.

## What was built

In framebuffer mode, when the process has a controlling TTY, the program:

1. Puts the TTY in **near-raw mode** (cfmakeraw *minus ISIG*) and saves
   the original `termios`, restoring it on exit.
2. Reads key bytes in `watchInput()` (a goroutine started from `Show()`,
   exiting on `w.closed` / read error):
   - `0x12` (Ctrl+R) → refresh callback, debounced
   - `0x14` (Ctrl+T) → rotate callback, debounced
   - everything else ignored (Alt/function keys arrive as ESC-prefixed
     sequences; unknown bytes are simply skipped)
3. Ctrl+C still raises SIGINT (ISIG kept) → the app's normal graceful
   shutdown.

## Design decisions (and why)

- **TTY, not evdev.** When the program owns the console (the systemd
  deployment in systemd-framebuffer.md), the TTY byte stream is simpler
  (no protocol parsing, no `/dev/input` permissions, no `input` group),
  and — unlike evdev — input is naturally scoped to our VT: we only get
  keys while our console is active, which is exactly when the image is
  visible. evdev would have delivered keyboard events from *every* VT
  regardless of which one was on screen.
- **Unilateral TTY input, PTY included.** The input reader starts for
  *any* controlling TTY, not just `/dev/ttyN`. Over SSH the controlling
  TTY is `/dev/pts/N`; Ctrl+R typed in that session reaches the program
  as `0x12` and triggers a refresh. VT-specific behaviors (cursor hiding,
  `TIOCGCONS` repaint-watch, `VT_ACTIVATE`/`-take-console`) remain gated
  on `/dev/ttyN` via the `isVT` check.
- **Debounce instead of key-state tracking.** The console byte stream has
  no press/release distinction — a held key auto-repeats bytes. A trigger
  within `triggerDebounce` (2s) of the previous one is dropped.
- **ISIG kept on purpose.** Full raw mode would eat Ctrl+C and break the
  graceful-shutdown path.
- **Oflag left alone on purpose.** Clearing `OPOST` (as cfmakeraw does)
  disables `ONLCR`, so `\n` stops becoming `\r\n` and log output drifts
  one column right per line. We only ever write plain logs + cursor
  escapes, so output processing stays enabled. (Live fix if you ever
  see it: `stty onlcr` on that console.)

## Known caveats

- While the program runs, the controlling TTY is raw: no line editing /
  echo in that terminal (expected; it's the kiosk's input). Restored on
  clean exit. `SIGKILL` leaves the TTY raw (`stty sane` fixes it, or
  reboot).
- Reading the TTY should not interfere with kernel VT switching
  (Ctrl+Alt+F* is handled by the VT layer before bytes reach a reader),
  but **this must be verified on hardware** (below).
- If stdin is redirected *and* there is no controlling TTY (daemon with
  `StandardInput=null`, no `TTYPath=`), there is no input at all — the
  signal fallback (SIGHUP/SIGUSR1 refresh, SIGUSR2 rotate) remains.

## Verification checklist (hardware, Pi)

1. `systemd-framebuffer.md` setup with `TTYPath=/dev/tty7` +
   `-take-console`. Boot → lands on the display.
2. On the kiosk console: Ctrl+R fetches a new screen (verbose:
   `Ctrl+R: manual refresh`); Ctrl+T cycles rotation.
3. Hold Ctrl+R for 5s → exactly one refresh (debounce).
4. **Escape works:** Ctrl+Alt+F1 still switches to the admin console.
   If it doesn't, that is a stop-the-world bug (kiosk trap).
5. Switch back (Ctrl+Alt+F7) → image repaints within ~0.5s (`watchVT`).
6. SSH: `ssh pi 'trmnl-framebuffer -output=framebuffer ...'` run from a
   *different* VT session → Ctrl+R typed over ssh triggers refresh.
7. Ctrl+C exits cleanly; afterwards the console is usable (termios
   restored) and the cursor is back.

## Why evdev was dropped (for the record)

The original plan (evdev reader on `/dev/input/eventN`, ~250 lines,
keycode 44/20/29/97, `EVIOCGBIT`/`EVIOCGKEY` discovery, no `EVIOCGRAB`)
was sound but unnecessary once the deployment model settled on "the
program owns its console": the TTY path covers the same two shortcuts
with ~100 lines, zero new permissions, and correct VT scoping. If a
future deployment needs input while *not* owning a VT (e.g. fb takeover
from another session on a multi-seat box), revisit evdev — the protocol
details from that plan remain in git history / earlier versions of this
file.
