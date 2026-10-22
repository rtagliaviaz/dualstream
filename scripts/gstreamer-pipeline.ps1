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
$required = @('videotestsrc','vp8enc','rtpvp8pay','udpsink','x264enc','mpegtsmux','hlssink')
foreach ($p in $required) {
    gst-inspect-1.0.exe $p 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Falta el plugin GStreamer '$p'." }
}

# ---- Wipe stale segments from previous runs ------------------------
# hlssink only enforces max-files while it's running. Files left over
# from a previous session would otherwise mix with the new run and make
# it look like max-files isn't being honored.
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
$tokens = @(
    '-e',

    # Source
    'videotestsrc', 'is-live=true', 'pattern=ball', '!',
    'video/x-raw,width=1280,height=720,framerate=30/1', '!',
    'tee', 'name=t',

    # --- WebRTC (VP8 -> UDP) ---
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

    # --- HLS 720p ---
    't.', '!', 'queue', '!',
        'x264enc', 'tune=zerolatency', 'bitrate=2000', 'key-int-max=30', 'speed-preset=ultrafast', '!',
        'h264parse', '!', 'mpegtsmux', '!',
        'hlssink',
        'target-duration=4', 'playlist-length=6', 'max-files=12',
        "location=$HlsRootFwd/720p/segment_%05d.ts",
        "playlist-location=$HlsRootFwd/720p/playlist.m3u8",

    # --- HLS 480p ---
    't.', '!', 'queue', '!', 'videoscale', '!',
        'video/x-raw,width=854,height=480', '!',
        'x264enc', 'tune=zerolatency', 'bitrate=800', 'key-int-max=30', 'speed-preset=ultrafast', '!',
        'h264parse', '!', 'mpegtsmux', '!',
        'hlssink',
        'target-duration=4', 'playlist-length=6', 'max-files=12',
        "location=$HlsRootFwd/480p/segment_%05d.ts",
        "playlist-location=$HlsRootFwd/480p/playlist.m3u8",

    # --- HLS 360p ---
    't.', '!', 'queue', '!', 'videoscale', '!',
        'video/x-raw,width=640,height=360', '!',
        'x264enc', 'tune=zerolatency', 'bitrate=400', 'key-int-max=30', 'speed-preset=ultrafast', '!',
        'h264parse', '!', 'mpegtsmux', '!',
        'hlssink',
        'target-duration=4', 'playlist-length=6', 'max-files=12',
        "location=$HlsRootFwd/360p/segment_%05d.ts",
        "playlist-location=$HlsRootFwd/360p/playlist.m3u8"
)

Write-Host "HLS root : $HlsRoot"
Write-Host "gst      : $gstExe"
Write-Host "Pipeline : $($tokens.Count) tokens"
Write-Host ""

# ---- Post-startup check in a background job ------------------------
# Waits 60s (well past max-files=12 * target-duration=4 = 48s) then
# reports whether hlssink is honoring max-files. This catches
# regressions in nightly GStreamer builds without you having to check
# manually.
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

    # If the user stopped GStreamer before 60s, just drop the job.
    # If it completed, print its result.
    if (Wait-Job $checkJob -Timeout 1) {
        Write-Host ""
        Write-Host "=== max-files check (after 60s of runtime) ==="
        Receive-Job $checkJob | Write-Host
    }
    Remove-Job $checkJob -Force -ErrorAction SilentlyContinue
}