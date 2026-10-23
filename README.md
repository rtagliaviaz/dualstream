# dualstream

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A WebRTC SFU with simulcast, dynamic layer switching, and an HLS fallback,
built from scratch with Pion and GStreamer.


## Table of Contents

- [What it does](#what-it-does)
- [Architecture](#architecture)
- [Components](#components)
  - [`cmd/sfu`](#cmdsfu)
  - [`cmd/broadcaster-gst`](#cmdbroadcaster-gst)
  - [`cmd/viewer`](#cmdviewer)
  - [`internal/hls`](#internalhls)
- [Running it](#running-it)
  - [Prerequisites](#prerequisites)
  - [Windows 11](#windows-11)
  - [Linux / macOS](#linux--macos)
  - [Manual GStreamer pipeline](#manual-gstreamer-pipeline)
- [Dynamic layer switching](#dynamic-layer-switching)
  - [How it works](#how-it-works)
  - [Switching logic](#switching-logic)
  - [Debug endpoints](#debug-endpoints)
- [HLS fallback](#hls-fallback)
  - [How the player chooses](#how-the-player-chooses)
  - [Endpoints](#endpoints)
  - [Configuration](#configuration)
  - [Latency](#latency)
  - [Storage hygiene](#storage-hygiene)
- [What happens under the hood during a switch](#what-happens-under-the-hood-during-a-switch)
- [Limitations](#limitations)
  - [UI layer detection requires distinct resolutions](#ui-layer-detection-requires-distinct-resolutions)
  - [Restarting any component requires restarting all](#restarting-any-component-requires-restarting-all)
  - [VP8 freeze on large resolution jumps](#vp8-freeze-on-large-resolution-jumps)
  - [HLS requires a shared directory](#hls-requires-a-shared-directory)
- [Future work](#future-work)
  - [Lifecycle management](#lifecycle-management)
  - [LL-HLS (Low-Latency HLS)](#ll-hls-low-latency-hls)
  - [HLS ingestion endpoint](#hls-ingestion-endpoint)
  - [Metrics and observability](#metrics-and-observability)
  - [`SetParameters`-based layer switching](#setparameters-based-layer-switching)
  - [Audio](#audio)
- [License](#license)

## What it does

`dualstream` is a minimal but functional Selective Forwarding Unit (SFU) that:

- Accepts **three simulcast layers** (`h`, `m`, `l`) from a broadcaster.
- Forwards **one layer per viewer**, based on the viewer's estimated
  bandwidth reported via RTCP REMB.
- **Switches layers dynamically** in response to bandwidth changes, without
  renegotiating the connection or interrupting the RTP stream.
- Serves an **HLS fallback** from the same source, so clients that cannot
  use WebRTC (legacy browsers, HTTP-only networks, smart TVs) can still
  watch the stream with a few seconds of extra latency.
- Serves an **HTML viewer** that attempts WebRTC first, falls back to HLS
  automatically, and lets the user toggle between the two modes manually.
- **Manages the broadcaster lifecycle** automatically: dead layers are
  detected after 5 s of silence, viewers on dead layers are closed, and
  a new broadcaster connection transparently replaces the old one without
  restarting the SFU process.

The broadcaster is a bridge that reads RTP packets from a GStreamer pipeline
running on the same machine. The GStreamer pipeline produces three VP8
encodings (WebRTC) and three H.264/MPEG-TS segmentations (HLS) of the same
source at different resolutions and bitrates, and sends the RTP layers to
the bridge over UDP while writing the HLS segments to a shared directory.

## Architecture

```
    GStreamer                        Bridge (Go)             SFU (Go)           Viewers
    ─────────                        ───────────             ────────           ───────

    videotestsrc
         │
         ├── h: 1280x720 ──VP8/RTP──► :5000 ─┐
         ├── m:  854x480 ──VP8/RTP──► :5001 ─┼──► WebRTC ──► demux by RID
         └── l:  640x360 ──VP8/RTP──► :5002 ─┘                   │
                                                                 │
                                                  ┌──────────────┼──────────────┐
                                                  ▼              ▼              ▼
                                              Viewer 1       Viewer 2       Viewer 3
                                              layer h        layer m        layer l

         └── 720p/480p/360p ──H.264/TS──► ./hls/  ──►  GET /hls/*  ──►  hls.js

```

The bridge attaches the RID and MID header extensions to each RTP packet
so the SFU can demux the three layers. Every viewer gets a single
`RTPSender` whose SSRC and sequence numbers are stable across layer
switches, so the browser sees one continuous stream even when the
underlying layer changes.

The HLS side is independent: `hlssink` rotates segments in `./hls/`, the
SFU serves them over `/hls/`, and the browser uses `hls.js` when WebRTC
is not available.

## Components

### `cmd/sfu`

The Selective Forwarding Unit. It:

- Accepts a broadcaster via `POST /broadcast` (SDP offer → answer).
- Accepts viewers via `POST /subscribe?rid=h|m|l`.
- Measures the actual bitrate of each layer once per second.
- Reads REMB feedback from each viewer and decides whether to switch layers.
- Serves the HTML viewer at `/`.
- Serves HLS content at `/hls/*` from a configurable directory
  (`-hls-dir` flag or `HLS_DIR` environment variable, default `./hls`).
- Exposes `GET /hls/info` with a JSON snapshot of available variants.
- Detects broadcaster death via per-layer inactivity timeout (5 s) and
  cleans up layers and viewers accordingly.
- Exposes `POST /reset` to force a clean slate without restarting the
  process.

### `cmd/broadcaster-gst`

A bridge that sits between GStreamer and the SFU. It:

- Listens on UDP ports `5000`, `5001`, `5002` for RTP packets.
- Rewrites the RTP header extensions (MID, RID) on each packet.
- Writes the packets into three `TrackLocalStaticRTP` tracks.
- Publishes all three tracks to the SFU over a single WebRTC connection.

### `cmd/viewer`

A CLI viewer for testing without a browser. Connects to the SFU, receives
one layer, and logs the RTP packets it receives.

### `internal/hls`

The HLS server package. It:

- Serves a generated master playlist (`/hls/master.m3u8`) that lists only
  the variants currently present on disk.
- Rewrites media playlists so that segment URIs resolve under `/hls/<variant>/`.
- Enforces path safety with two independent checks: the first path
  component must be a whitelisted variant name (rejecting `..`, `etc`,
  and unknown names), and the second component must not contain `/` or
  `\` (rejecting anything that would escape the variant directory). The
  whitelist alone is sufficient to prevent traversal; no `path.Clean`
  or `filepath.Clean` trickery is needed.

  
## Running it

### Prerequisites

- **Go 1.25+**
- **GStreamer 1.22+** with the following plugins: `videotestsrc`,
  `vp8enc`, `rtpvp8pay`, `udpsink`, `x264enc`, `h264parse`, `mpegtsmux`,
  `hlssink`. On Linux these come from `gstreamer1.0-plugins-{base,good,bad,ugly}`.
  On Windows, install the *runtime* and *development* `.msi` packages from
  [gstreamer.freedesktop.org](https://gstreamer.freedesktop.org/download/#windows).
- **A modern browser** (Chrome, Edge, Firefox, Safari) with `hls.js`
  available via CDN.

### Windows 11

Four terminals, always in this order:

**Terminal 1 — GStreamer pipeline:**

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
.\scripts\gstreamer-pipeline.ps1
```

This runs the pipeline as a token array (not a single quoted string) to
work around a PowerShell 5.1 bug that mangles `!` when passed to native
executables. It also wipes stale `.ts`/`.m3u8` files on startup, so
`max-files` is enforced from a clean slate.

**Terminal 2 — SFU:**

```powershell
$env:HLS_DIR = "$PWD\hls"
go run .\cmd\sfu
```

**Terminal 3 — Bridge:**

```powershell
go run .\cmd\broadcaster-gst
```

**Terminal 4 — Browser:**

Open `http://localhost:8080/`. The player tries WebRTC first, falls back
to HLS automatically if WebRTC fails within 6 seconds, and exposes a
toggle button to switch modes manually.


### Linux / macOS

The same four terminals apply, using the equivalent shell script:

**Terminal 1 — GStreamer pipeline:**

```bash
./scripts/gstreamer-pipeline.sh
```

**Terminal 2 — SFU:**

```bash
HLS_DIR=./hls go run ./cmd/sfu
```

**Terminal 3 — Bridge:**

```bash
go run ./cmd/broadcaster-gst
```

**Terminal 4 — Browser:**

Open `http://localhost:8080/`.

### Manual GStreamer pipeline

If you prefer to run GStreamer directly without the wrapper script, this
is the full pipeline the scripts produce:

```bash
gst-launch-1.0 -e \
  videotestsrc is-live=true pattern=ball ! \
  video/x-raw,width=1280,height=720,framerate=30/1 ! \
  tee name=t \
  t. ! queue ! vp8enc deadline=1 cpu-used=4 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5000 \
  t. ! queue ! videoscale ! video/x-raw,width=854,height=480 ! vp8enc deadline=1 cpu-used=4 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5001 \
  t. ! queue ! videoscale ! video/x-raw,width=640,height=360 ! vp8enc deadline=1 cpu-used=4 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5002 \
  t. ! queue ! x264enc tune=zerolatency bitrate=2000 key-int-max=30 speed-preset=ultrafast ! h264parse ! mpegtsmux ! \
      hlssink target-duration=4 playlist-length=6 max-files=12 location=./hls/720p/segment_%05d.ts playlist-location=./hls/720p/playlist.m3u8 \
  t. ! queue ! videoscale ! video/x-raw,width=854,height=480 ! x264enc tune=zerolatency bitrate=800 key-int-max=30 speed-preset=ultrafast ! h264parse ! mpegtsmux ! \
      hlssink target-duration=4 playlist-length=6 max-files=12 location=./hls/480p/segment_%05d.ts playlist-location=./hls/480p/playlist.m3u8 \
  t. ! queue ! videoscale ! video/x-raw,width=640,height=360 ! x264enc tune=zerolatency bitrate=400 key-int-max=30 speed-preset=ultrafast ! h264parse ! mpegtsmux ! \
      hlssink target-duration=4 playlist-length=6 max-files=12 location=./hls/360p/segment_%05d.ts playlist-location=./hls/360p/playlist.m3u8
```

Note the `-e` flag: it ensures `hlssink` receives an EOS on `Ctrl+C`
and closes the playlist cleanly.

## Dynamic layer switching

### How it works

The SFU subscribes to REMB (Receiver Estimated Maximum Bitrate) feedback
from each viewer. REMB is an RTCP message where the browser tells the
server how much bandwidth it thinks it can handle. The SFU uses that
number to decide which layer to forward.

Every second, the SFU also measures the **actual bitrate** of each layer
by counting bytes read from the broadcaster's tracks. This is important:
the static fallback values (`h=2 Mbps`, `m=1 Mbps`, `l=500 kbps`) are
only used when a layer has not seen traffic yet. Once measurement starts,
the decision uses real numbers.

### Switching logic

Downscale and upscale are asymmetric:

- **Downscale** requires the estimated bitrate to drop below
  `current_layer_bitrate × 0.8`. The SFU then tries one step at a time
  (`h → m`, then `m → l` if still too low), so it does not jump
  straight to the lowest layer when only a moderate downgrade is
  needed.
- **Upscale** requires the estimated bitrate to exceed
  `target_layer_bitrate × 1.5`. The SFU jumps directly to the highest
  layer that fits, since the cooldown prevents oscillation.

A 10-second cooldown between switches prevents rapid oscillation when
the estimated bitrate hovers around a threshold.

### Debug endpoints

For testing without a real network, the SFU exposes:

```
POST /debug/bitrate?bps=N
```

This forces the estimated bitrate to `N` for all viewers, overriding the
REMB feedback. Use `bps=0` to clear the override and return to real REMB.

You can also inspect the current state of each layer with:

```
GET /layers
```

Example output:

```
rid  measured_bps  viewers  last_pkt
h    500000        1        0s
m    250000        0        0s
l    125000        0        0s
```

The `last_pkt` column shows the time since the last RTP packet was
received on that layer. Layers that go 5 s without packets are removed
by the lifecycle monitor.

And the HLS availability with:

```
GET /hls/info
```

Example output:

```json
{
  "available": true,
  "master": "/hls/master.m3u8",
  "variants": [
    {"name":"720p","bandwidth":2200000,"width":1280,"height":720},
    {"name":"480p","bandwidth":900000,"width":854,"height":480},
    {"name":"360p","bandwidth":450000,"width":640,"height":360}
  ]
}
```

## HLS fallback

The SFU serves a complete HLS distribution of the same stream. This lets
clients that cannot use WebRTC (HTTP-only networks, legacy browsers,
smart TVs) still watch, at the cost of a few seconds of extra latency.

### How the player chooses

The `player.js` frontend:

1. Tries WebRTC first by calling `POST /subscribe?rid=h` with a JSON SDP
   offer.
2. If WebRTC fails (HTTP error, ICE failure, or a 6-second timeout
   without a track), it calls `GET /hls/info`. If `available` is `true`,
   it tears down WebRTC and starts HLS playback with `hls.js`.
3. A toggle button lets the user force either mode. The button label
   always describes the *next* action ("Cambiar a HLS" when in WebRTC,
   "Cambiar a WebRTC" when in HLS), and the badge next to the title shows
   the current mode in color (green = WebRTC, orange = HLS).

The player sends SDP as JSON (`{type, sdp}`), not as raw text. This
matches the Go server, which uses `json.Decoder` on `/broadcast` and
`/subscribe`.

### Endpoints

| Method | Path                           | Description                        |
|--------|--------------------------------|------------------------------------|
| GET    | `/hls/master.m3u8`             | Master playlist                    |
| GET    | `/hls/<variant>/playlist.m3u8` | Media playlist (rewritten URIs)    |
| GET    | `/hls/<variant>/segment_*.ts`  | MPEG-TS segments                   |
| GET    | `/hls/info`                    | JSON snapshot of HLS availability  |

### Configuration

| Knob              | Where                     | Default           | Meaning                              |
|-------------------|---------------------------|-------------------|--------------------------------------|
| `HLS_DIR`         | env / `-hls-dir` flag     | `./hls`           | Directory served as `/hls/*`         |
| `target-duration` | GStreamer                 | 4 s               | Segment length                       |
| `playlist-length` | GStreamer                 | 6                 | Segments in the playlist (~24 s)     |
| `max-files`       | GStreamer                 | 12                | Max `.ts` files on disk per variant  |
| `bitrate`         | GStreamer                 | 2000/800/400 kbps | Per-variant bitrate                  |

The directory must be shared between the GStreamer process and the SFU
(same machine or a mounted volume in Docker / Kubernetes).

### Latency

HLS is live, not VOD: the playlist updates continuously and the SFU
never adds `#EXT-X-ENDLIST`. But unlike WebRTC, HLS introduces a
constant latency floor:

| Mode   | Typical latency |
|--------|-----------------|
| WebRTC | < 1 s           |
| HLS    | 6–30 s          |

The offset is constant over time (viewing hour 1 means you are ~8 s
behind, not progressively falling behind). Reducing it below ~4 s
requires LL-HLS (Low-Latency HLS), which is out of scope for this demo.

### Storage hygiene

`hlssink` enforces `max-files=12` **only while the process is running**.
Between runs, old `.ts` files remain on disk and can mix with new ones
with the same names. The provided scripts (`gstreamer-pipeline.ps1` and
`gstreamer-pipeline.sh`) wipe `./hls/*/` on startup so `max-files` is
always respected from a clean slate. If you run GStreamer manually, do
the same cleanup yourself, or expect the number of `.ts` files to grow
across runs.

## What happens under the hood during a switch

The naive way to switch layers is `RTPSender.ReplaceTrack()`. It does not
work well with VP8 in Chrome: every switch changes the SSRC, and the
browser interprets the new SSRC as a new stream, freezes the decoder,
and waits for missing packets that will never arrive.

`dualstream` uses a different approach:

1. **The `outTrack` of each viewer never changes.** It is created once
   when the viewer subscribes and lives for the entire connection.
2. **Every RTP packet is rewritten per viewer.** The `SSRC`,
   `sequence number`, and `timestamp` fields are replaced with values
   from the viewer's own counters. This makes each viewer see a single
   continuous stream, regardless of which layer is being read.
3. **Frame boundaries are respected.** When the SFU switches layers, it
   sets a flag that drops packets until the next `marker=true` (end of
   frame). The next packet after that marker is a clean frame start.
4. **Keyframes are requested on the target layer** via PLI, so the
   first frame the browser decodes after a switch is a complete
   keyframe.

The result is a switch that the browser does not perceive as a stream
change, only as a change in resolution and image quality.

## Limitations

### UI layer detection requires distinct resolutions

The HTML viewer detects layer changes by watching the `resize` event on
the `<video>` element. That event fires whenever the decoded resolution
changes, which in turn happens when the SFU starts forwarding a layer
with a different resolution.

This works because each layer has a distinct resolution:

| Layer | Resolution |
|-------|------------|
| `h`   | 1280×720   |
| `m`   | 854×480    |
| `l`   | 640×360    |

If all three layers share the same resolution (a design known as
"bitrate layers" or "temporal layers"), the UI cannot detect the
switch because the video element's resolution does not change. The SFU
still switches correctly, but the buttons and status line would stay
stuck on the initial layer.

Two ways to fix this in the future:

1. **Poll the SFU.** Add a `GET /whoami` endpoint that returns the
   current layer of a viewer, and have the UI poll it every second.
2. **Server-to-client notification.** Open a DataChannel between the SFU
   and each viewer and send a message when the layer changes. More
   complex but no polling.

### Restarting any component still requires restarting all (mostly)

The SFU now detects broadcaster death automatically: after 5 s without
packets on a layer, the layer is killed, its viewers' peer connections
are closed, and the browser falls back to HLS. A new broadcaster
connection replaces the old one and migrates viewers transparently.

What the SFU still cannot do:

- **Revive a bridge or GStreamer pipeline** that died silently. The SFU
  sees the death (timeout) but cannot force the bridge to reconnect. You
  still need to restart the bridge and/or GStreamer manually.
- **Guarantee state consistency across everything.** The bridge and
  GStreamer each maintain their own state (SSRCs, sequence counters,
  encoder state). If only one of them is restarted, the SFU will detect
  the resulting silence and kill the layer, but you'll still want to
  restart the whole stack to avoid SSRC collisions and stale counters.

**The recommended workflow is still:** whenever you restart any
component, restart all four in this order:

1. SFU (or use `POST /reset` instead of killing it)
2. Bridge
3. GStreamer
4. Browser viewer

If you're iterating only on the browser side, `POST /reset` followed by
a page refresh is enough — no need to touch the terminals.

### VP8 freeze on large resolution jumps

With some browsers (notably Chrome), switching VP8 from a low
resolution directly to a much higher one can cause a brief decoder
freeze while the browser reallocates buffers. The freeze is brief
(typically under 500 ms) but visible.

Two mitigations are implemented:

- `keyframe-max-dist=1` in the GStreamer pipeline, so every frame is a
  keyframe and the decoder has a clean restart point immediately.
- Frame boundary alignment in the SFU, so the browser never sees a
  mixed frame across two resolutions.

For production, the correct fix is to use `SetParameters` with three
persistent tracks per viewer instead of a single track whose content
changes. That is how LiveKit and Mediasoup handle simulcast and it
eliminates the freeze entirely.

### HLS requires a shared directory

The SFU and GStreamer must agree on a directory. On a single machine
that is trivial. For a distributed deployment (GStreamer on one host,
SFU on another), you need either a shared volume, an object-store
backend for the `hls.Server`, or a `POST /hls/upload` endpoint to push
segments from the generator to the SFU. None of these is implemented
yet.


## Future work

### Broadcaster auto-reconnect

The SFU detects a dead broadcaster (5 s inactivity or a failed/closed
peer connection) and kills its layers, but it cannot force the bridge
process to reconnect. Adding exponential-backoff reconnect logic inside
`cmd/broadcaster-gst` would close that gap. Roughly 60 lines.

### LL-HLS (Low-Latency HLS)

Reduce the HLS latency from ~8 s to ~2 s using partial segments
(`#EXT-X-PART`), preload hints, and blocking playlist reloads. Requires
`hlssink3` (from `gst-plugins-rs`) and `hls.js` ≥ 1.4 with
`lowLatencyMode: true`.

### HLS ingestion endpoint

Add a `POST /hls/upload` endpoint (or object-store backend) so
GStreamer can run on a different host from the SFU. The `hls.Server` is
already structured so that swapping `os.ReadFile` + `http.ServeFile` for
an interface (`Storage`) is straightforward.

### Metrics and observability

Expose per-layer and per-viewer metrics (bitrate, packet loss, jitter,
freeze count, HLS request rate) via a `/metrics` endpoint, so a
monitoring system could scrape them. The current implementation logs
only at the server and displays a subset in the viewer.

### `SetParameters`-based layer switching

Replace the current single-track approach with three persistent tracks
per viewer, toggled via `RTPSender.SetParameters`. This eliminates the
VP8 freeze entirely and is how production SFUs do it.

### Audio

The demo is video-only. Adding an Opus track to the WebRTC side and an
AAC track to the HLS side is the next logical step for a usable
streaming platform.

## License

MIT