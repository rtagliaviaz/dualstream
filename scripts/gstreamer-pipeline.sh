#!/usr/bin/env bash
#
# Generate the dual WebRTC + HLS pipeline on Linux and macOS.
# Mirrors scripts/gstreamer-pipeline.ps1 in behavior:
#   - preflight checks plugins,
#   - detects an AAC encoder (avenc_aac or voaacenc),
#   - wipes stale .ts/.m3u8 before running,
#   - runs a 60s max-files sanity check in the background.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

HLS_ROOT="${HLS_ROOT:-$REPO_ROOT/hls}"
mkdir -p "$HLS_ROOT/720p" "$HLS_ROOT/480p" "$HLS_ROOT/360p"

# ---- Preflight ---------------------------------------------------------
if ! command -v gst-launch-1.0 >/dev/null 2>&1; then
    {
        echo "error: gst-launch-1.0 not found in PATH"
        echo "       install GStreamer 1.22+ with the required plugins:"
        echo "         Debian/Ubuntu: sudo apt install gstreamer1.0-tools \\"
        echo "           gstreamer1.0-plugins-{base,good,bad,ugly} gstreamer1.0-libav"
        echo "         macOS (Homebrew): brew install gstreamer gst-plugins-{base,good,bad,ugly}"
    } >&2
    exit 1
fi

for plugin in videotestsrc vp8enc rtpvp8pay udpsink \
              x264enc h264parse mpegtsmux hlssink \
              audiotestsrc opusenc rtpopuspay aacparse \
              audioconvert audioresample; do
    if ! gst-inspect-1.0 "$plugin" >/dev/null 2>&1; then
        echo "error: GStreamer plugin '$plugin' not found" >&2
        echo "       install it (Debian/Ubuntu: gstreamer1.0-plugins-{base,good,bad,ugly} gstreamer1.0-libav)" >&2
        exit 1
    fi
done

# AAC encoder: prefer avenc_aac (from gst-libav), fall back to voaacenc (from gst-plugins-bad).
AAC_ENC=""
for cand in avenc_aac voaacenc; do
    if gst-inspect-1.0 "$cand" >/dev/null 2>&1; then
        AAC_ENC="$cand"
        break
    fi
done
if [ -z "$AAC_ENC" ]; then
    echo "error: no AAC encoder found (need avenc_aac from gst-libav or voaacenc from gst-plugins-bad)" >&2
    exit 1
fi
if [ "$AAC_ENC" != "avenc_aac" ]; then
    echo "warning: avenc_aac not available; using $AAC_ENC" >&2
fi

# ---- Wipe stale segments ----------------------------------------------
wiped=0
shopt -s nullglob
for v in 720p 480p 360p; do
    for f in "$HLS_ROOT/$v"/*.ts "$HLS_ROOT/$v"/*.m3u8; do
        rm -f "$f"
        wiped=$((wiped + 1))
    done
done
shopt -u nullglob
echo "Old segments wiped: $wiped files"
echo "HLS root: $HLS_ROOT"
echo "AAC enc : $AAC_ENC"
echo

# ---- Background max-files sanity check --------------------------------
(
    sleep 60
    echo
    echo "=== max-files check (after 60s of runtime) ==="
    for v in 720p 480p 360p; do
        shopt -s nullglob
        files=("$HLS_ROOT/$v"/*.ts)
        count=${#files[@]}
        shopt -u nullglob
        if [ "$count" -gt 12 ]; then
            status="FAIL (max-files not honored)"
        else
            status="OK"
        fi
        printf '%-6s %3d .ts files   %s\n' "$v" "$count" "$status"
    done
) &
checker_pid=$!

cleanup() {
    kill "$checker_pid" 2>/dev/null || true
    wait "$checker_pid" 2>/dev/null || true
}
trap cleanup EXIT

# ---- Run GStreamer from the repo root so relative paths resolve ------
cd "$REPO_ROOT"
gst-launch-1.0 -e \
    audiotestsrc is-live=true wave=sine freq=440 volume=0.15 ! \
    audioconvert ! audioresample ! audio/x-raw,rate=48000,channels=2 ! \
    tee name=asrc \
    asrc. ! queue ! opusenc bitrate=64000 ! rtpopuspay pt=111 ! \
        udpsink host=127.0.0.1 port=5004 \
    asrc. ! queue ! audioconvert ! "$AAC_ENC" bitrate=128000 ! aacparse ! \
        tee name=aac \
    videotestsrc is-live=true pattern=ball ! \
    video/x-raw,width=1280,height=720,framerate=30/1 ! \
    tee name=t \
    t. ! queue ! vp8enc deadline=1 cpu-used=4 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5000 \
    t. ! queue ! videoscale ! video/x-raw,width=854,height=480 ! vp8enc deadline=1 cpu-used=4 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5001 \
    t. ! queue ! videoscale ! video/x-raw,width=640,height=360 ! vp8enc deadline=1 cpu-used=4 ! rtpvp8pay pt=96 ! udpsink host=127.0.0.1 port=5002 \
    t. ! queue ! x264enc tune=zerolatency bitrate=2000 key-int-max=30 speed-preset=ultrafast ! \
        h264parse ! mpegtsmux name=mux720 \
    aac. ! queue ! mux720. \
    mux720. ! hlssink target-duration=4 playlist-length=6 max-files=12 \
        location="$HLS_ROOT/720p/segment_%05d.ts" \
        playlist-location="$HLS_ROOT/720p/playlist.m3u8" \
    t. ! queue ! videoscale ! video/x-raw,width=854,height=480 ! \
        x264enc tune=zerolatency bitrate=800 key-int-max=30 speed-preset=ultrafast ! \
        h264parse ! mpegtsmux name=mux480 \
    aac. ! queue ! mux480. \
    mux480. ! hlssink target-duration=4 playlist-length=6 max-files=12 \
        location="$HLS_ROOT/480p/segment_%05d.ts" \
        playlist-location="$HLS_ROOT/480p/playlist.m3u8" \
    t. ! queue ! videoscale ! video/x-raw,width=640,height=360 ! \
        x264enc tune=zerolatency bitrate=400 key-int-max=30 speed-preset=ultrafast ! \
        h264parse ! mpegtsmux name=mux360 \
    aac. ! queue ! mux360. \
    mux360. ! hlssink target-duration=4 playlist-length=6 max-files=12 \
        location="$HLS_ROOT/360p/segment_%05d.ts" \
        playlist-location="$HLS_ROOT/360p/playlist.m3u8"