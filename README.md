# 3djuggler

A daemon that turns a 3D printer into a shared, network-managed resource.

`3djuggler` runs on a small device next to a printer. It polls a web endpoint
for queued print jobs, waits for someone to physically confirm at the machine,
then streams the G-code to the printer over serial, reporting progress as it
goes.

It was written for a hackathon in 2018 to replace a paper queue taped to the
wall next to two office printers. It is still in production today, driving
printers across dozens of offices.

## Why a daemon and not a web UI on the printer

The design constraint was that a printer sits on an office network, and the
people using it do not. Rather than exposing anything per printer, the daemon
only ever dials out:

```
  person → web UI → job queue ← 3djuggler → printer
                                (polls)     (serial/USB)
```

Nothing reaches into the office. There is no inbound port, no firewall
exception and no per-printer DNS record to maintain, and the poll doubles as
the liveness heartbeat. That narrow contract, poll HTTP out and write serial
in, is why the same binary has outlived three generations of the hardware it
runs on.

## What you need to run it

* A 3D printer that accepts G-code over a serial connection. Tested against
  Prusa MK3/MK3S (including MMU2) and MK4.
* A machine to run the daemon on, connected to the printer by USB. Anything
  that runs Go will do; in production this is a small ARM panel.
* A backend implementing the job API below.

## Job API

The daemon is the client, so you supply the server. Every call is a POST and
must include `app` and `token` credentials.

### `POST /job/`

| `action` | Purpose |
| --- | --- |
| `get` | Return the next queued job, or the job named by `id` |
| `update` | Record a new `status` for job `id` |
| `delete` | Remove job `id` from the queue |

A `get` responds with:

```json
{
  "Success": true,
  "Content": {
    "id": 42,
    "file_name": "bracket.gcode",
    "file_content": "G28\nG1 X10\n...",
    "owner": "someone",
    "status": "New",
    "color": "Galaxy Black"
  }
}
```

Return `"Content": null` or an `id` of `0` when the queue is empty.

### `POST /printer/`

| `action` | Purpose |
| --- | --- |
| `heartbeat` | Mark the printer online; sent on every poll |
| `reschedule` | Reset the confirmation timer for the current job |

## Local control API

The daemon also serves a small HTTP API on `localhost:8888`, which is what a
touchscreen next to the printer talks to. `/start`, `/pause` and `/cancel`
wait for the daemon to apply the state transition before responding.

| Endpoint | Purpose |
| --- | --- |
| `GET /info` | Current job as JSON: owner, file name, status, progress |
| `GET /start` | Confirm and start the job, or resume a paused one |
| `GET /pause` | Pause the running job |
| `GET /cancel` | Cancel the running job |
| `GET /reschedule` | Give the owner more time to confirm |
| `GET /version` | Build commit, as plain text |

## Job lifecycle

```
Waiting for job → Waiting for a button → Sending to printer → Printing → Finished
                          ↓                                       ↓
                   Button timeout                               Paused
```

A job does not start until a person presses the button at the machine, which
they have ten minutes to do. Miss it and the job moves to the back of the
queue rather than being discarded, so nobody loses their place by being late,
and nobody can hold a printer they are not standing next to.

## Build

```sh
go build ./...
```

`/version` reports the commit the binary was built from. The Go toolchain
records it automatically, so there is nothing to pass at build time. Building
from a tree with uncommitted changes appends `-dirty`.

Cross-compiling for the device is a normal Go cross-compile:

```sh
GOOS=linux GOARCH=arm64 go build .
```

## Run

```sh
3djuggler -config 3djuggler.json
```

| Flag | Default | Purpose |
| --- | --- | --- |
| `-config` | `3djuggler.json` | Config file path |
| `-log` | `/var/log/3djuggler.log` | Log file path |
| `-verbose` | `false` | Debug-level logging |
| `-insecure` | `false` | Skip TLS verification. Development only |

See `3djuggler.json` for a complete example config.

Note that the config key is spelled `InternEnpoint`. The typo is from the
original 2018 commit and is kept deliberately, because every deployed config
in the fleet still uses it.

## Development

`fakejuggler/` is an interactive stub that serves the local control API
without any hardware, for developing a client against.

`gcodefeeder/` is the serial-facing half and is usable on its own, for any
device that takes G-code over a serial line.

```sh
go test -race ./...
```

## License

Apache 2.0, see [LICENSE](LICENSE).

I am providing code in the repository to you under an open source license.
Because this is my personal repository, the license you receive to my code is
from me and not my employer (Meta).
