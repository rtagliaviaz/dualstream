#Requires -Version 5.1
$ErrorActionPreference = "Stop"

# ---- Paths ---------------------------------------------------------
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))

$HlsRoot = if ($env:HLS_ROOT) { $env:HLS_ROOT } else { Join-Path $repoRoot "hls" }
$HlsRoot = [System.IO.Path]::GetFullPath($HlsRoot)
foreach ($v in '720p','480p','360p') {
    New-Item -ItemType Directory -Force -Path (Join-Path $HlsRoot $v) | Out-Null
}
# Forward slashes para location=; PS no las manipula
$HlsRootFwd = $HlsRoot.Replace('\','/')

$gstExe = (Get-Command gst-launch-1.0.exe -ErrorAction Stop).Source

# ---- Plugin preflight ----------------------------------------------
$required = @(
    'videotestsrc','vp8enc','rtpvp8pay','udpsink',
    'x264enc','h264parse','mpegtsmux','hlssink',
    'audiotestsrc','opusenc','rtpopuspay','aacparse',
    'audioconvert','audioresample'
)
foreach ($p in $required) {
    gst-inspect-1.0.exe $p 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "Missing GStreamer plugin: '$p'"
    }
}

# AAC encoder: prefer avenc_aac (from gst-libav), fall back to voaacenc (from gst-plugins-bad).
$AacEnc = $null
foreach ($cand in @('avenc_aac','voaacenc')) {
    gst-inspect-1.0.exe $cand 2>$null | Out-Null
    if ($LASTEXITCODE -eq 0) {
        $AacEnc = $cand
        break
    }
}
if (-not $AacEnc) {
    throw "No AAC encoder found. Install gst-libav (avenc_aac) or gst-plugins-bad (voaacenc)."
}
if ($AacEnc -ne 'avenc_aac') {
    Write-Host "avenc_aac not available; using $AacEnc" -ForegroundColor Yellow
}

# ---- Wipe stale segments from previous runs ------------------------
$wiped = 0
foreach ($v in '720p','480p','360p') {
    $dir = Join-Path $HlsRoot $v
    Get-ChildItem -Path $dir -Filter '*.ts'   -File -ErrorAction SilentlyContinue |
        ForEach-Object { Remove-Item $_.FullName -Force; $script:wiped++ }
    Get-ChildItem -Path $dir -Filter '*.m3u8' -File -ErrorAction SilentlyContinue |
        ForEach-Object { Remove-Item $_.FullName -Force; $script:wiped++ }
}
Write-Host ("Old segments wiped: {0} files" -f $wiped)

# ---- Pipeline as token array (fixes PowerShell 5.1 quoting bug) ----
# NOTE: the audio tree runs in parallel with the video tree:
#   audiotestsrc ─┬─ opusenc ─ rtpopuspay ─ udpsink:5004   (WebRTC audio)
#                 └─ aacenc ─ aacparse ─ tee ─┬─ mux720    (HLS audio 720p)
#                                             ├─ mux480    (HLS audio 480p)
#                                             └─ mux360    (HLS audio 360p)
$tokens = @(
    '-e',

    # ---- Audio source ----
    'audiotestsrc', 'is-live=true', 'wave=sine', 'freq=440', 'volume=0.15', '!',
    'audioconvert', '!',
    'audioresample', '!',
    'audio/x-raw,rate=48000,channels=2', '!',
    'tee', 'name=asrc',

    # WebRTC audio (Opus → RTP → UDP:5004)
    'asrc.', '!', 'queue', '!',
        'opusenc', 'bitrate=64000', '!',
        'rtpopuspay', 'pt=111', '!',
        'udpsink', 'host=127.0.0.1', 'port=5004',

    # HLS audio (AAC encoded once, fanned out to the three muxers)
    'asrc.', '!', 'queue', '!',
        'audioconvert', '!',
        $AacEnc, 'bitrate=128000', '!',
        'aacparse', '!',
        'tee', 'name=aac',

    # ---- Video source ----
    'videotestsrc', 'is-live=true', 'pattern=ball', '!',
    'video/x-raw,width=1280,height=720,framerate=30/1', '!',
    'tee', 'name=t',

    # WebRTC video (3 simulcast layers, unchanged)
    't.', '!', 'queue', '!',
        'vp8enc', 'deadline=1', 'cpu-used=4', '!',
        'rtpvp8pay', 'pt=96', '!',
        'udpsink', 'host=127.0.0.1', 'port=5000',
    't.', '!', 'queue', '!', 'videoscale', '!',
        'video/x-raw,width=854,height=480', '!',
        'vp8enc', 'deadline=1', 'cpu-used=4', '!',
        'rtpvp8pay', 'pt=96', '!',
        'udpsink', 'host=127.0.0.1', 'port=5001',
    't.', '!', 'queue', '!', 'videoscale', '!',
        'video/x-raw,width=640,height=360', '!',
        'vp8enc', 'deadline=1', 'cpu-used=4', '!',
        'rtpvp8pay', 'pt=96', '!',
        'udpsink', 'host=127.0.0.1', 'port=5002',

    # HLS 720p: video → mux720, audio → mux720, mux720 → hlssink
    't.', '!', 'queue', '!',
        'x264enc', 'tune=zerolatency', 'bitrate=2000', 'key-int-max=30', 'speed-preset=ultrafast', '!',
        'h264parse', '!',
        'mpegtsmux', 'name=mux720',
    'aac.', '!', 'queue', '!', 'mux720.',
    'mux720.', '!', 'hlssink',
        'target-duration=4', 'playlist-length=6', 'max-files=12',
        "location=$HlsRootFwd/720p/segment_%05d.ts",
        "playlist-location=$HlsRootFwd/720p/playlist.m3u8",

    # HLS 480p
    't.', '!', 'queue', '!', 'videoscale', '!',
        'video/x-raw,width=854,height=480', '!',
        'x264enc', 'tune=zerolatency', 'bitrate=800', 'key-int-max=30', 'speed-preset=ultrafast', '!',
        'h264parse', '!',
        'mpegtsmux', 'name=mux480',
    'aac.', '!', 'queue', '!', 'mux480.',
    'mux480.', '!', 'hlssink',
        'target-duration=4', 'playlist-length=6', 'max-files=12',
        "location=$HlsRootFwd/480p/segment_%05d.ts",
        "playlist-location=$HlsRootFwd/480p/playlist.m3u8",

    # HLS 360p
    't.', '!', 'queue', '!', 'videoscale', '!',
        'video/x-raw,width=640,height=360', '!',
        'x264enc', 'tune=zerolatency', 'bitrate=400', 'key-int-max=30', 'speed-preset=ultrafast', '!',
        'h264parse', '!',
        'mpegtsmux', 'name=mux360',
    'aac.', '!', 'queue', '!', 'mux360.',
    'mux360.', '!', 'hlssink',
        'target-duration=4', 'playlist-length=6', 'max-files=12',
        "location=$HlsRootFwd/360p/segment_%05d.ts",
        "playlist-location=$HlsRootFwd/360p/playlist.m3u8"
)

Write-Host "HLS root : $HlsRoot"
Write-Host "gst      : $gstExe"
Write-Host "AAC enc  : $AacEnc"
Write-Host "Pipeline : $($tokens.Count) tokens"
Write-Host ""

# ---- Background max-files sanity check -----------------------------
$checkJob = Start-Job -ScriptBlock {
    param($root)
    Start-Sleep -Seconds 60
    $out = @()
    foreach ($v in '720p','480p','360p') {
        $files = Get-ChildItem (Join-Path $root "$v\*.ts") -File -ErrorAction SilentlyContinue
        $count = if ($files) { $files.Count } else { 0 }
        $status = if ($count -gt 12) { 'FAIL (max-files not honored)' } else { 'OK' }
        $out += "{0,-6} {1,3} .ts files   {2}" -f $v, $count, $status
    }
    $out -join "`n"
} -ArgumentList $HlsRoot

# ---- Run GStreamer -------------------------------------------------
Push-Location $repoRoot
try {
    & $gstExe @tokens
} finally {
    Pop-Location
    if (Wait-Job $checkJob -Timeout 1) {
        Write-Host ""
        Write-Host "=== max-files check (after 60s of runtime) ==="
        Receive-Job $checkJob | Write-Host
    }
    Remove-Job $checkJob -Force -ErrorAction SilentlyContinue
}