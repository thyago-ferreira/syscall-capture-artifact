# Hook point — send system call of cs-emu (empirically confirmed)

All capture tools must hook the same system call and use the same event
criterion; otherwise the comparison is not commensurable. This was confirmed by
measurement, not assumed.

## Method

Build `cs-emu` (Go), run a local receiver on 127.0.0.1:9100, and trace threads
and the candidate send system calls:

```
strace -f -e trace=network,write,writev -s 80 -o hook.strace \
  cs-emu --dest 127.0.0.1:9100 --size 6300 --rate 5 --ramp 1 --conn per-event --duration 1.5s
```

Origin host: Debian 12, kernel 6.1, x86_64.

## Result

- **The body is sent with `write(2)`**, not `sendto`, `sendmsg`, `sendmmsg` or
  `writev`. The Go runtime (`net.TCPConn.Write` over a single `[]byte`) maps to
  `write(2)` on the connected TCP socket.
- **One call per event (1:1):** 9 events sent → exactly 9 `write` calls with the
  event prefix. No fragmentation at the application level.
- **Whole buffer in a single call:** each call returns the full length
  (envelope of about 130 B + body of 6,300 B), i.e., `n == len(buf)`.
- In per-event mode a new socket is opened for each event (`socket`/`connect`
  before, `close` after).

Representative line:
```
write(6, "POST /events/vehicles HTTP/1.0\r\nHost: cs-emulator.local\r\nContent-Type: application"..., 6432) = 6432
```

(The measurements in the paper were taken with an endpoint path two bytes
shorter, i.e., 6,430-byte writes; the 6,300-byte body is identical.)

## Fixed decisions (apply to strace, kprobe and fentry)

1. **Single target system call: `write`.** All tools hook the `write(2)` path
   (x86_64: tracepoint `sys_enter_write`/`sys_exit_write`; kprobe on
   `ksys_write`/`vfs_write`/`__x64_sys_write`; fentry on the corresponding BTF
   function). The exact attach function is an implementation detail of each
   tool; the logical system call and the event it carries are fixed.
2. **Uniform event criterion: prefix `POST /events/vehicles`.** The process also
   uses `write(2)` for other purposes (stderr logging, the `--lat-out` dump at
   the end), so filtering by system call is not enough: only buffers that start
   with that request line are events (same rule as `EVENT_PREFIX` in
   `parse_strace.py`). The body is delimited by the envelope's `Content-Length`
   (split at `\r\n\r\n`).
3. **Kernel-level hook, not libc.** Go issues the `syscall` instruction
   directly, without libc, so libc-symbol interception (LD_PRELOAD) sees nothing.

## Validity note

This is the behavior of the Go emulator. The runtime of the reference
constituent system may issue `writev`, `sendto` or `sendmsg` instead of `write`.
The hook point fixed here holds for the testbed (identical load under the same
system call); the emulator reproduces the load profile, not necessarily the
system-call pattern of the original runtime.
