<#
.SYNOPSIS
  Installs the KloudIT Recon host agent on this Windows PC.

.DESCRIPTION
  - Copies recon-host.exe / recon-hostw.exe to the install directory, plus
    recon-encoder.exe (the native capture/encode helper) when the bundle has it
  - Downloads FFmpeg (BtbN GPL release build, SHA-256 verified) unless -FFmpegPath is given
  - Optionally downloads FFmpeg's LGPL shared libraries for the encoder helper's libavcodec
    backend (Intel Quick Sync Video), SHA-256 verified (-InstallLibavcodec)
  - Optionally pairs with your gateway (-PairingCode)
  - Registers a logon task that runs the agent hidden, with highest privileges
    (needed to send input to elevated games/launchers), restarting on failure
  - Opens the direct-path UDP port in Windows Firewall (Private/Domain only,
    scoped to the agent executable)
  - Optionally installs the ViGEmBus driver for virtual Xbox controllers
  - Optionally installs the Virtual Display Driver (pinned release, SHA-256 verified) so
    sessions can stream a virtual monitor at the client's resolution and frame rate

  Run from an elevated PowerShell in the folder containing the binaries (the
  unzipped bundle, or the install directory to re-pair or update settings):
    powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -PairingCode "recon1:..."

.PARAMETER PairingCode
  Code shown by the gateway's "Add a PC" dialog (starts with recon1:). Can also
  be applied later with recon-host.exe pair <code>; a running agent picks it up.
.PARAMETER InstallDir
  Where to install (default: Program Files\KlouditRecon). The logon task runs the agent from
  there elevated, so a folder outside Program Files is restricted to administrators (owner
  Administrators, users read and run), with everything already in it; a folder that is or
  holds a link (junction, symbolic link) is refused.
.PARAMETER FFmpegPath
  Use an existing ffmpeg.exe (FFmpeg 7.1+; 8.1+ recommended, older builds lack gfxcapture). The
  agent runs elevated and runs it only from a folder that only administrators can change, such
  as Program Files.
.PARAMETER DirectPort
  UDP port for direct LAN connections from the browser (0 disables the direct path). The
  default, 48100, stays clear of Sunshine's and Apollo's ports (47984-48010). Earlier versions
  used 47998; running this installer again moves host.json and the firewall rule to 48100.
.PARAMETER UpdateFFmpeg
  Download FFmpeg again even if it is already installed (with -InstallLibavcodec: its libraries too).
.PARAMETER InstallLibavcodec
  Download BtbN's FFmpeg 8.1 LGPL shared build (ffmpeg-n8.1-latest-win64-lgpl-shared-8.1.zip,
  SHA-256 verified like the FFmpeg download) and put avcodec-62.dll, avutil-60.dll and
  swresample-6.dll with its LICENSE.txt into <InstallDir>\ffmpeg-lgpl, where recon-encoder.exe
  loads them for its libavcodec backend: hardware encoding with Intel Quick Sync Video
  (h264_qsv, hevc_qsv, av1_qsv) on GPUs without an AMF or NVENC backend (docs/VENDOR_NOTES.md,
  3.8). LGPL (libvpl, which Quick Sync needs, is MIT and built into avcodec-62.dll); the GPL
  ffmpeg.exe above stays the FFmpeg command-line path and is never loaded by the helper.
  About 80 MB to download, 95 MB installed.
.PARAMETER InstallViGEm
  Install the ViGEmBus driver with winget (virtual Xbox controllers).
.PARAMETER InstallVirtualDisplay
  Install the Virtual Display Driver (github.com/VirtualDrivers/Virtual-Display-Driver
  release 25.7.23, an IddCx driver signed by SignPath Foundation) with nefcon v1.14.0, both
  pinned by SHA-256, and set "virtualDisplay": "auto" in host.json unless it is set already.
  Sessions then stream a virtual monitor at the client's resolution and frame rate whenever
  the physical monitor cannot show them 1:1: another size (with the client's default
  resolution "Native", its screen size) or a frame rate above its refresh rate. During the
  stream it is the primary display ("virtualDisplayLayout": primary; extend or only), and
  it stays "virtualDisplayLinger" seconds (10) after the stream before the previous layout
  is restored. Set "virtualDisplay": "off" in host.json to stream the monitor instead.
  Test the driver alone with recon-host.exe vdisplay (docs/INSTALL.md, step 7). Windows
  asks once whether to trust the publisher "SignPath Foundation". When the Virtual Display
  Driver or SudoVDA (installed by Apollo) is already present nothing is installed, and the
  setting is made for it.
.PARAMETER NoStart
  Do not start the agent after installing.
#>
[CmdletBinding()]
param(
    [string]$PairingCode,
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'KlouditRecon'),
    [string]$FFmpegPath,
    [ValidateRange(0, 65535)][int]$DirectPort = 48100,
    [switch]$UpdateFFmpeg,
    [switch]$InstallLibavcodec,
    [switch]$InstallViGEm,
    [switch]$InstallVirtualDisplay,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$TaskName = 'KloudIT Recon Host'
$RuleName = 'KloudIT Recon host (direct path)'

function Write-Step([string]$msg) { Write-Host "==> $msg" -ForegroundColor Cyan }

# BtbN's FFmpeg builds (https://github.com/BtbN/FFmpeg-Builds, release "latest"): file name ->
# SHA-256 from the release's checksums.sha256.
$BtbNBase = 'https://github.com/BtbN/FFmpeg-Builds/releases/download/latest'
function Get-BtbNChecksums {
    $sums = (Invoke-WebRequest -UseBasicParsing -Uri "$BtbNBase/checksums.sha256").Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
    $known = @{}
    foreach ($l in ($sums -split "`n")) {
        $parts = $l.Trim() -split '\s+'
        if ($parts.Count -eq 2) { $known[$parts[1]] = $parts[0].ToLowerInvariant() }
    }
    $known
}
# The oldest release build of a flavour (gpl, lgpl-shared, ...) from FFmpeg 8.1 on (and before
# $below, when given).
function Select-BtbNBuild($known, [string]$flavour, [string]$below = '') {
    $known.Keys | Where-Object { $_ -match "^ffmpeg-n(\d+\.\d+)-latest-win64-$flavour-\1\.zip$" -and
        [version]$Matches[1] -ge [version]'8.1' -and (-not $below -or [version]$Matches[1] -lt [version]$below) } |
        Sort-Object { [version]($_ -replace '^ffmpeg-n(\d+\.\d+)-.*$', '$1') } | Select-Object -First 1
}
# Downloads a BtbN build into $tmp, checks its SHA-256 and unpacks it there; returns the
# archive's top directory.
function Expand-BtbNBuild([string]$zipName, [string]$expected, [string]$tmp) {
    $zip = Join-Path $tmp $zipName
    Invoke-WebRequest -UseBasicParsing -Uri "$BtbNBase/$zipName" -OutFile $zip
    $actual = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLowerInvariant()
    if ($expected -ne $actual) { throw "Checksum mismatch for $zipName (expected $expected, got $actual)." }
    Write-Step "$zipName checksum verified"
    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $inner = Get-ChildItem -Path $tmp -Directory | Where-Object { Test-Path (Join-Path $_.FullName 'bin') } | Select-Object -First 1
    if (-not $inner) { throw "Unexpected archive layout in $zipName." }
    $inner.FullName
}

# Reads what the agent appended to its log since offset $from (the agent keeps
# the file open, so share it).
function Read-LogSince([string]$path, [long]$from) {
    try {
        $fs = [IO.File]::Open($path, 'Open', 'Read', 'ReadWrite, Delete')
        try {
            if ($fs.Length -lt $from) { $from = 0 }
            [void]$fs.Seek($from, 'Begin')
            (New-Object IO.StreamReader($fs)).ReadToEnd()
        } finally { $fs.Dispose() }
    } catch { '' }
}

# Folders the elevated agent loads programs, libraries or settings from must be writable by
# administrators only. A folder made under C:\ (or another drive's root) would inherit
# "Authenticated Users: Modify" from the drive root, so any local account could replace what
# runs elevated there. Reparse points (junctions, symbolic links) in it would let whoever made
# them redirect it.
function Assert-NoLinks([string]$dir, [switch]$Recurse) {
    $root = Get-Item -LiteralPath $dir -Force
    if (($root.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "$($root.FullName) is a link (reparse point); remove it and run the installer again."
    }
    foreach ($c in @(Get-ChildItem -LiteralPath $dir -Force)) {
        if (($c.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "$($c.FullName) is a link (reparse point); remove it and run the installer again."
        }
        if ($Recurse -and $c.PSIsContainer) { Assert-NoLinks $c.FullName -Recurse }
    }
}
# Creates $dir if needed and gives it to administrators: owner Administrators, no inherited
# access, Administrators and SYSTEM full control, Users read and execute. What is in it from
# before (its files; with -Recurse its folders too, all the way down) becomes owned by
# Administrators with only that access.
function Protect-AdminFolder([string]$dir, [switch]$Recurse) {
    if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir | Out-Null }
    Assert-NoLinks $dir -Recurse:$Recurse
    icacls $dir /setowner '*S-1-5-32-544' | Out-Null
    if ($LASTEXITCODE -eq 0) {
        icacls $dir /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-545:(OI)(CI)RX' | Out-Null
    }
    if ($LASTEXITCODE -ne 0) { throw "Could not restrict access to $dir (icacls exit code $LASTEXITCODE)." }
    Assert-NoLinks $dir -Recurse:$Recurse # again, now that only administrators can add entries
    $items = if ($Recurse) { @(Get-ChildItem -LiteralPath $dir -Force) } else { @(Get-ChildItem -LiteralPath $dir -Force -File) }
    foreach ($f in $items) {
        $tree = @()
        if ($f.PSIsContainer) { $tree = @('/T') }
        icacls $f.FullName /setowner '*S-1-5-32-544' @tree | Out-Null
        if ($LASTEXITCODE -eq 0) { icacls $f.FullName /reset @tree | Out-Null }
        if ($LASTEXITCODE -ne 0) { throw "Could not restrict access to $($f.FullName) (icacls exit code $LASTEXITCODE)." }
    }
}
# Whether $dir is inside Program Files, whose access Windows already limits to administrators.
function Test-InProgramFiles([string]$dir) {
    $full = [IO.Path]::GetFullPath($dir).TrimEnd('\') + '\'
    foreach ($pf in @($env:ProgramFiles, ${env:ProgramFiles(x86)}) | Where-Object { $_ }) {
        if ($full.StartsWith([IO.Path]::GetFullPath($pf).TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)) { return $true }
    }
    $false
}

# --- Preconditions -----------------------------------------------------------
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this script from an elevated PowerShell (Run as administrator).'
}
if (-not [Environment]::Is64BitOperatingSystem) { throw '64-bit Windows is required.' }

$src = $PSScriptRoot
foreach ($f in 'recon-host.exe', 'recon-hostw.exe') {
    if (-not (Test-Path (Join-Path $src $f))) { throw "$f not found next to this script." }
}

if ($PairingCode -and $PairingCode.Trim() -notmatch '^recon1:[A-Za-z0-9_-]+$') {
    throw 'The pairing code must be the recon1:... text from the "Add a PC" dialog, nothing else.'
}

# --- Binaries ----------------------------------------------------------------
Write-Step "Installing agent to $InstallDir"
Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
# Stop the agent and any encoder it started (ffmpeg / recon-encoder can outlive it
# for a moment), and wait for them to exit so their files can be replaced.
$old = @(Get-Process -Name 'recon-hostw', 'recon-host' -ErrorAction SilentlyContinue) +
    @(Get-Process -Name 'ffmpeg', 'recon-encoder' -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$InstallDir\*" })
$old | Stop-Process -Force -ErrorAction SilentlyContinue
$old | Wait-Process -Timeout 10 -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
# The logon task runs the agent from here elevated, and the agent starts recon-encoder.exe,
# FFmpeg and the helper's FFmpeg libraries from here: outside Program Files, only
# administrators may change the folder (see Protect-AdminFolder).
if (-not (Test-InProgramFiles $InstallDir)) {
    Protect-AdminFolder $InstallDir -Recurse
    Write-Step "Restricted $InstallDir to administrators (users read and run)"
}
# Skip files that are already in place (when re-run from the install directory).
# recon-encoder.exe is optional: without it the agent uses the FFmpeg path.
foreach ($f in 'recon-host.exe', 'recon-hostw.exe', 'recon-encoder.exe', 'install-host.ps1', 'uninstall-host.ps1') {
    $p = Join-Path $src $f
    if ((Test-Path $p) -and ((Resolve-Path $p).Path -ne (Join-Path $InstallDir $f))) { Copy-Item $p $InstallDir -Force }
}

# --- FFmpeg ------------------------------------------------------------------
if ($FFmpegPath) {
    if (-not (Test-Path $FFmpegPath)) { throw "FFmpeg not found at $FFmpegPath" }
    $ffmpeg = (Resolve-Path $FFmpegPath).Path
    # The agent runs elevated, so it runs FFmpeg only from its own folder or a folder only
    # administrators can change (it checks the owner and ACL), else it ignores this path.
    $inInstallDir = $ffmpeg.StartsWith([IO.Path]::GetFullPath($InstallDir).TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)
    if (-not $inInstallDir -and -not (Test-InProgramFiles (Split-Path $ffmpeg))) {
        Write-Warning "The agent runs FFmpeg only from a folder that only administrators can change (Program Files, or one with such an ACL), and otherwise ignores $ffmpeg (host.log: host config `"ffmpeg`" ignored). Without -FFmpegPath the installer puts FFmpeg into $InstallDir."
    }
} else {
    $ffDir = Join-Path $InstallDir 'ffmpeg'
    $ffmpeg = Join-Path $ffDir 'bin\ffmpeg.exe'
    if ($UpdateFFmpeg -or -not (Test-Path $ffmpeg)) {
        $known = Get-BtbNChecksums
        # The oldest FFmpeg 8.1+ release build: it has both GPU capture paths (ddagrab and
        # gfxcapture, new in 8.1) and works with the widest range of GPU drivers (the nightly
        # "master" build can require an NVIDIA driver released a few weeks ago).
        $zipName = Select-BtbNBuild $known 'gpl'
        if (-not $zipName) {
            Write-Warning 'No FFmpeg 8.1+ release build is listed; falling back to the nightly master build.'
            $zipName = 'ffmpeg-master-latest-win64-gpl.zip'
        }
        $expected = $known[$zipName]
        if (-not $expected) { throw 'Could not find the FFmpeg checksum.' }
        Write-Step "Downloading FFmpeg ($zipName, about 200 MB; this can take a few minutes)"
        $tmp = Join-Path $env:TEMP ("recon-ffmpeg-" + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        try {
            $inner = Expand-BtbNBuild $zipName $expected $tmp
            if (-not (Test-Path (Join-Path $inner 'bin\ffmpeg.exe'))) { throw 'Unexpected FFmpeg archive layout.' }
            if (Test-Path $ffDir) { Remove-Item -Recurse -Force $ffDir }
            Copy-Item $inner $ffDir -Recurse  # Move-Item can't cross drives in PowerShell 5.1
        } finally {
            Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
        }
    }
}
Write-Step "FFmpeg: $ffmpeg"

# --- libavcodec for the encoder helper (optional) ---------------------------------
# recon-encoder.exe's libavcodec backend (Intel Quick Sync Video, GUIDE 3.8) loads FFmpeg's
# shared libraries at run time from ffmpeg-lgpl\ next to it; without them it reports the
# backend unavailable. The LGPL shared build: libvpl (MIT) is built into avcodec-62.dll.
if ($InstallLibavcodec) {
    $libDir = Join-Path $InstallDir 'ffmpeg-lgpl'
    if ($UpdateFFmpeg -or -not (Test-Path (Join-Path $libDir 'avcodec-62.dll'))) {
        $known = Get-BtbNChecksums
        # FFmpeg 8.x only: the helper is built for libavcodec 62 / libavutil 60 (no master fallback).
        $zipName = Select-BtbNBuild $known 'lgpl-shared' '9.0'
        if (-not $zipName) { throw 'No FFmpeg 8.x LGPL shared release build is listed by BtbN/FFmpeg-Builds.' }
        Write-Step "Downloading FFmpeg's libraries ($zipName, about 80 MB)"
        $tmp = Join-Path $env:TEMP ("recon-libav-" + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        try {
            $inner = Expand-BtbNBuild $zipName $known[$zipName] $tmp
            $dlls = @(Get-ChildItem -Path (Join-Path $inner 'bin') -File | Where-Object { $_.Name -match '^(avcodec|avutil|swresample)-\d+\.dll$' })
            if (-not ($dlls | Where-Object { $_.Name -eq 'avcodec-62.dll' }) -or -not ($dlls | Where-Object { $_.Name -eq 'avutil-60.dll' })) {
                throw "$zipName has no avcodec-62.dll / avutil-60.dll (FFmpeg 8.x)."
            }
            if (Test-Path $libDir) { Remove-Item -Recurse -Force $libDir }
            New-Item -ItemType Directory -Force -Path $libDir | Out-Null
            $dlls | Copy-Item -Destination $libDir
            Copy-Item (Join-Path $inner 'LICENSE.txt') $libDir -ErrorAction SilentlyContinue
        } finally {
            Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
        }
    }
    Write-Step "libavcodec for the encoder helper: $libDir"
}

# --- Virtual display driver (optional) -------------------------------------------
# Pinned downloads: the Virtual Display Driver's driver-only package and nefcon (devcon-style
# root device installer, the tool the driver's own silent installer uses).
$vdd = @{
    Url    = 'https://github.com/VirtualDrivers/Virtual-Display-Driver/releases/download/25.7.23/VirtualDisplayDriver-x86.Driver.Only.zip'
    Sha256 = 'e24210692b442b39af763536330ce78b423f19342b7a7792c26de3944e418b3a'
}
$nefcon = @{
    Url    = 'https://github.com/nefarius/nefcon/releases/download/v1.14.0/nefcon_v1.14.0.zip'
    Sha256 = 'a15557da24a9efca203158de3b43b0eaf982db231f0194031f1ed428bc13e669'
}
# The driver reads its modes from vdd_settings.xml in this folder (VDDPATH), and the elevated agent
# adds each client's mode to it: only administrators and SYSTEM may change the folder and its
# files (users, among them the driver's LocalService host, read them; Protect-AdminFolder). The
# agent refuses to edit a folder that non-administrators can change.
function Get-VddFolder {
    $p = (Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\MikeTheTech\VirtualDisplayDriver' -Name VDDPATH -ErrorAction SilentlyContinue).VDDPATH
    if ($p) { $p } else { 'C:\VirtualDisplayDriver' }
}
$virtualDisplayReady = $false
if ($InstallVirtualDisplay) {
    $present = @(Get-PnpDevice -PresentOnly -ErrorAction SilentlyContinue | Where-Object {
        $_.HardwareID -contains 'Root\MttVDD' -or $_.HardwareID -contains 'root\sudomaker\sudovda' })
    if ($present) {
        Write-Step "Virtual display driver already installed: $($present[0].FriendlyName) ($($present[0].InstanceId))"
        $vddPresent = @($present | Where-Object { $_.HardwareID -contains 'Root\MttVDD' })
        if ($vddPresent) {
            $vddDir = Get-VddFolder
            if (Test-Path -LiteralPath $vddDir) {
                Protect-AdminFolder $vddDir
                Write-Step "Restricted $vddDir to administrators (users read)"
            }
            if ($vddPresent[0].Status -eq 'OK') {
                Write-Host '    Its device is enabled, so its monitor stays connected between sessions. Disable it in Device Manager > Display adapters to have sessions enable it only while they stream.'
            }
        }
        $virtualDisplayReady = $true
    } else {
        Write-Step 'Installing the Virtual Display Driver 25.7.23'
        $tmp = Join-Path $env:TEMP ("recon-vdd-" + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        try {
            foreach ($d in $vdd, $nefcon) {
                $zip = Join-Path $tmp ([IO.Path]::GetFileName($d.Url))
                Invoke-WebRequest -UseBasicParsing -Uri $d.Url -OutFile $zip
                $actual = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLowerInvariant()
                if ($actual -ne $d.Sha256) { throw "Checksum mismatch for $($d.Url) (expected $($d.Sha256), got $actual)." }
                Expand-Archive -Path $zip -DestinationPath $tmp -Force
            }
            Write-Step 'Virtual Display Driver and nefcon checksums verified'
            $inf = Join-Path $tmp 'VirtualDisplayDriver\MttVDD.inf'
            $cat = Join-Path $tmp 'VirtualDisplayDriver\mttvdd.cat'
            $nefconc = Join-Path $tmp 'x64\nefconc.exe'
            foreach ($f in $inf, $cat, $nefconc) { if (-not (Test-Path $f)) { throw "Unexpected archive layout: $f is missing." } }
            $sig = Get-AuthenticodeSignature -FilePath $cat
            if ($sig.Status -ne 'Valid' -or $sig.SignerCertificate.Subject -notmatch 'O=SignPath Foundation') {
                throw "The driver catalog's signature is $($sig.Status) ($($sig.SignerCertificate.Subject)), expected a valid SignPath Foundation signature."
            }
            # The driver's settings (modes): the agent adds each client's mode when needed.
            $vddDir = Get-VddFolder
            Protect-AdminFolder $vddDir
            if (-not (Test-Path -LiteralPath (Join-Path $vddDir 'vdd_settings.xml'))) {
                Copy-Item (Join-Path $tmp 'VirtualDisplayDriver\vdd_settings.xml') $vddDir
            }
            Write-Host '    Windows Security may ask whether to install software from "SignPath Foundation": choose Install.'
            $out = & { $ErrorActionPreference = 'Continue'; & $nefconc install $inf 'Root\MttVDD' 2>&1 } | ForEach-Object { "$_" } | Out-String
            $code = $LASTEXITCODE
            Write-Host $out.Trim()
            if ($code -eq 3010) {
                Write-Warning 'The Virtual Display Driver is installed; Windows needs a reboot before it works.'
            } elseif ($code -ne 0) {
                throw "Installing the Virtual Display Driver failed (nefcon exit code $code)."
            }
            # Leave the device disabled: running, it keeps a monitor connected that Windows extends
            # the desktop onto. A session enables it while it streams and disables it after.
            foreach ($dev in @(Get-PnpDevice -ErrorAction SilentlyContinue | Where-Object { $_.HardwareID -contains 'Root\MttVDD' })) {
                try {
                    Disable-PnpDevice -InstanceId $dev.InstanceId -Confirm:$false -ErrorAction Stop
                    Write-Step "Virtual Display Driver device $($dev.InstanceId) disabled (sessions enable it while they stream)"
                } catch {
                    Write-Warning "Could not disable the Virtual Display Driver device $($dev.InstanceId) ($_). Disable it in Device Manager > Display adapters, or its monitor stays connected between sessions."
                }
            }
            $virtualDisplayReady = $true
        } finally {
            Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
        }
    }
}

# --- Configuration (per user: the agent runs in your interactive session) ----
$cfgDir = Join-Path $env:APPDATA 'KlouditRecon'
$cfgPath = Join-Path $cfgDir 'host.json'
$logPath = Join-Path $cfgDir 'host.log'
New-Item -ItemType Directory -Force -Path $cfgDir | Out-Null
$cfg = [ordered]@{}
if (Test-Path $cfgPath) {
    $existing = Get-Content -Raw -Encoding UTF8 $cfgPath | ConvertFrom-Json
    foreach ($p in $existing.PSObject.Properties) { $cfg[$p.Name] = $p.Value }
}
$cfg['ffmpeg'] = $ffmpeg
$cfg['directPort'] = $DirectPort
if (-not $cfg.Contains('audio')) { $cfg['audio'] = $true }
if (-not $cfg.Contains('gamepad')) { $cfg['gamepad'] = $true }
if ($virtualDisplayReady -and -not $cfg.Contains('virtualDisplay')) {
    $cfg['virtualDisplay'] = 'auto'
    Write-Step 'host.json: "virtualDisplay": "auto" (a stream the monitor cannot show 1:1 gets a virtual monitor, the primary display while it runs; "off" turns it off)'
}
# UTF-8 without a byte-order mark (Set-Content -Encoding UTF8 adds one in PowerShell 5.1).
[IO.File]::WriteAllText($cfgPath, ($cfg | ConvertTo-Json -Depth 5), (New-Object Text.UTF8Encoding $false))
# Owner-only access: the file holds the host token.
# (SIDs instead of names: "Administrators" is localised on non-English Windows.)
icacls $cfgDir /inheritance:r /grant:r "${env:USERNAME}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Warning "Could not restrict access to $cfgDir (icacls exit code $LASTEXITCODE)." }

$exe = Join-Path $InstallDir 'recon-host.exe'
$exeW = Join-Path $InstallDir 'recon-hostw.exe'
if ($PairingCode) {
    Write-Step 'Pairing with gateway'
    & $exe -config $cfgPath pair $PairingCode.Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Pairing failed.' }
}

# --- Firewall ------------------------------------------------------------------
Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
if ($DirectPort -gt 0) {
    Write-Step "Allowing inbound UDP $DirectPort for the direct path (Private/Domain networks)"
    New-NetFirewallRule -DisplayName $RuleName -Direction Inbound -Action Allow -Protocol UDP `
        -LocalPort $DirectPort -Program $exeW -Profile Private, Domain | Out-Null
    foreach ($n in @(Get-NetConnectionProfile -ErrorAction SilentlyContinue | Where-Object { "$($_.NetworkCategory)" -eq 'Public' })) {
        Write-Warning ("Network '$($n.Name)' ($($n.InterfaceAlias)) is set to Public, so the direct path is blocked on it " +
            "and streams go through the gateway instead (a few ms more latency). If it is your home network, run: " +
            "Set-NetConnectionProfile -InterfaceIndex $($n.InterfaceIndex) -NetworkCategory Private")
    }
}

# --- Logon task ----------------------------------------------------------------
Write-Step "Registering logon task '$TaskName'"
$user = "$env:USERDOMAIN\$env:USERNAME"
$action = New-ScheduledTaskAction -Execute $exeW -Argument ("-config `"$cfgPath`" -log `"$logPath`" run")
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$taskPrincipal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
$settings.Priority = 4
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $taskPrincipal -Settings $settings -Force | Out-Null

# --- ViGEmBus --------------------------------------------------------------------
if ($InstallViGEm) {
    if (Get-Command winget -ErrorAction SilentlyContinue) {
        Write-Step 'Installing ViGEmBus (virtual Xbox controllers)'
        winget install --id ViGEm.ViGEmBus -e --accept-package-agreements --accept-source-agreements
    } else {
        Write-Warning 'winget not found. Install ViGEmBus manually: https://github.com/nefarius/ViGEmBus/releases'
    }
}

# --- Probe & start -----------------------------------------------------------------
Write-Step 'Detected capabilities'
# 'Continue' here: in PowerShell 5.1, stderr output from a native command would otherwise stop the script.
$probe = & { $ErrorActionPreference = 'Continue'; & $exe -config $cfgPath probe 2>&1 } | ForEach-Object { "$_" } | Out-String
Write-Host $probe
if ($probe -match 'minimum required Nvidia driver[^|\r\n]*') {
    Write-Warning "NVENC needs a newer NVIDIA driver ($($Matches[0].Trim())). Update it (NVIDIA App or nvidia.com), then run Stop-ScheduledTask '$TaskName'; Start-ScheduledTask '$TaskName'."
}
if ($probe -notmatch '(?m)^encoder:\s+\S+\s+\S+\s+(nvidia|amd|intel)') {
    Write-Warning 'No GPU encoder works, so video will be encoded on the CPU (higher latency). Install or update the graphics driver; the "unusable:" lines above say why each GPU encoder failed.'
}
# The native encoder helper is the default video pipeline on AMD and NVIDIA (the "helper:" lines
# above); without a usable backend sessions stream through FFmpeg's command line instead.
if ($probe -match '(?m)^helper:\s+(no usable encoder|does not run|not installed)') {
    Write-Warning ('The native encoder helper (recon-encoder.exe) cannot encode on this PC, so streams use FFmpeg: ' +
        'a lost frame then costs a key frame and bitrate changes restart the encoder. The "helper:" and "unavailable:" lines above say why; ' +
        "update the graphics driver (NVIDIA: 570 or newer), then run Stop-ScheduledTask '$TaskName'; Start-ScheduledTask '$TaskName'.")
}

$paired = [bool](Get-Content -Raw -Encoding UTF8 $cfgPath | ConvertFrom-Json).gateway
if (-not $NoStart) {
    $logStart = if (Test-Path $logPath) { (Get-Item $logPath).Length } else { 0 }
    Start-ScheduledTask -TaskName $TaskName
    Write-Step 'Starting the agent'
    $state = 'slow'
    $seen = $false
    $started = Get-Date
    $deadline = $started.AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 500
        $log = Read-LogSince $logPath $logStart
        if ($log -match 'connected to gateway') { $state = 'connected'; break }
        if ($log -match 'rejected registration') { $state = 'rejected'; break }
        if (-not $paired -and $log -match 'not paired') { $state = 'unpaired'; break }
        $running = [bool](Get-Process -Name 'recon-hostw' -ErrorAction SilentlyContinue)
        if ($running) { $seen = $true }
        # Exited: it ran and is gone, or never appeared within 15 s.
        if (-not $running -and ($seen -or (Get-Date) -gt $started.AddSeconds(15))) { $state = 'exited'; break }
    }
    $recent = (($log -split "`r?`n") | Where-Object { $_ } | Select-Object -Last 8) -join "`n"
    switch ($state) {
        'connected' { Write-Step 'Agent is running and connected to the gateway.' }
        'unpaired'  { Write-Step "Agent is running and waiting to be paired: & '$exe' pair <code>" }
        'rejected'  { Write-Warning "The gateway rejected this PC's pairing code (the PC was re-paired or removed in the dashboard). Use Manage > Re-pair on its card and run the command it shows." }
        'exited'    { Write-Warning "The agent stopped. Last log lines ($logPath):`n$recent" }
        default     { Write-Warning "The agent is running but has not reached the gateway yet (is UDP to the gateway's port open?). Last log lines ($logPath):`n$recent" }
    }
}

Write-Host ''
Write-Host 'KloudIT Recon host agent installed.' -ForegroundColor Green
Write-Host "  Config: $cfgPath"
Write-Host "  Log:    $logPath"
if (-not $paired) {
    Write-Host "  Next:   & '$exe' -config '$cfgPath' pair <code-from-gateway>  (the running agent picks it up)"
}
Write-Host '  Tip:    for unattended use enable automatic sign-in and disable the lock screen: the agent'
Write-Host '          runs in your desktop session and cannot capture the Windows lock screen or UAC prompts.'
