# dualstream

A WebRTC SFU with simulcast, dynamic layer switching, and HLS fallback.

## Components

- `cmd/sfu` — the Selective Forwarding Unit. Receives simulcast layers from
  a broadcaster and forwards the appropriate layer to each viewer.
- `cmd/broadcaster-gst` — a bridge that reads RTP packets from a GStreamer
  pipeline and feeds them into a WebRTC PeerConnection.
- `cmd/viewer` — a CLI viewer for testing without a browser.


## Running

1. Start the SFU:

```bash
go run ./cmd/sfu
```

2. Start the GStreamer pipeline (in another terminal):

```bash
gst-launch-1.0 -v filesrc location="video.mp4" ! decodebin name=d
d. ! queue ! videoconvert ! video/x-raw,format=I420 ! tee name=v
v. ! queue ! videoscale ! video/x-raw,width=1280,height=720 ! vp8enc deadline=33 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5000
v. ! queue ! videoscale ! video/x-raw,width=854,height=480 ! vp8enc deadline=33 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5001
v. ! queue ! videoscale ! video/x-raw,width=640,height=360 ! vp8enc deadline=33 keyframe-max-dist=30 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5002
```

3. Start the bridge:

```bash
go run ./cmd/broadcaster-gst
```

4. Open the viewer in a browser:

http://localhost:8080/

