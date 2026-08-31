- shouldn't the status bar show battery / wifi info too?  what does the non-framebuffer status bar show?

- what does epaper mode actually do?

- framebuffer info log line

- could we lock our framebuffer image to a specific TTY and disable getty on that TTY?  would that actually help with things like the cursor blink and ability to still use other VTs?

- verify that with our VT / systemd setup, that output going to journald doesn't stop us from detecting the TTY correctly

- would it be possible to seed the "model" size and details (colors, etc) from the framebuffer itself?
