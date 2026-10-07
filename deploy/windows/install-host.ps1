<#
.SYNOPSIS
  Installs the KloudIT Recon host agent on this Windows PC.

.DESCRIPTION
  - Copies recon-host.exe / recon-hostw.exe to the install directory, plus
    recon-encoder.exe (the native capture/encode helper) when the bundle has it
  - Downloads FFmpeg (BtbN GPL release build, SHA-256 verified) unless -FFmpegPath is given
  - Optionally pairs with your gateway (-PairingCode)
  - Registers a logon task that runs the agent hidden, with highest privileges
    (needed to send input to elevated games/launchers), restarting on failure
  - Opens the direct-path UDP port in Windows Firewall (Private/Domain only,
    scoped to the agent executable)
  - Optionally installs the ViGEmBus driver for virtual Xbox controllers

  Run from an elevated PowerShell in the folder containing the binaries (the
  unzipped bundle, or the install directory to re-pair or update settings):
    powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -PairingCode "recon1:..."

.PARAMETER PairingCode
  Code shown by the gateway's "Add a PC" dialog (starts with recon1:). Can also
  be applied later with recon-host.exe pair <code>; a running agent picks it up.
.PARAMETER InstallDir
  Where to install (default: Program Files\KlouditRecon).
.PARAMETER FFmpegPath
  Use an existing ffmpeg.exe (FFmpeg 7.1+; 8.1+ recommended, older builds lack gfxcapture).
.PARAMETER DirectPort
  UDP port for direct LAN connections from the browser (0 disables the direct path).
.PARAMETER UpdateFFmpeg
  Download FFmpeg again even if it is already installed.
.PARAMETER InstallViGEm
  Install the ViGEmBus driver with winget (virtual Xbox controllers).
.PARAMETER NoStart
  Do not start the agent after installing.
#>
[CmdletBinding()]
param(
    [string]$PairingCode,
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'KlouditRecon'),
    [string]$FFmpegPath,
    [ValidateRange(0, 65535)][int]$DirectPort = 47998,
    [switch]$UpdateFFmpeg,
    [switch]$InstallViGEm,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$TaskName = 'KloudIT Recon Host'
$RuleName = 'KloudIT Recon host (direct path)'

function Write-Step([string]$msg) { Write-Host "==> $msg" -ForegroundColor Cyan }

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
} else {
    $ffDir = Join-Path $InstallDir 'ffmpeg'
    $ffmpeg = Join-Path $ffDir 'bin\ffmpeg.exe'
    if ($UpdateFFmpeg -or -not (Test-Path $ffmpeg)) {
        $base = 'https://github.com/BtbN/FFmpeg-Builds/releases/download/latest'
        $sums = (Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.sha256").Content
        if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
        $known = @{}
        foreach ($l in ($sums -split "`n")) {
            $parts = $l.Trim() -split '\s+'
            if ($parts.Count -eq 2) { $known[$parts[1]] = $parts[0].ToLowerInvariant() }
        }
        # The oldest FFmpeg 8.1+ release build: it has both GPU capture paths (ddagrab and
        # gfxcapture, new in 8.1) and works with the widest range of GPU drivers (the nightly
        # "master" build can require an NVIDIA driver released a few weeks ago).
        $zipName = $known.Keys | Where-Object { $_ -match '^ffmpeg-n(\d+\.\d+)-latest-win64-gpl-\1\.zip$' -and [version]$Matches[1] -ge [version]'8.1' } |
            Sort-Object { [version]($_ -replace '^ffmpeg-n(\d+\.\d+)-.*$', '$1') } | Select-Object -First 1
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
            $zip = Join-Path $tmp $zipName
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$zipName" -OutFile $zip
            $actual = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLowerInvariant()
            if ($expected -ne $actual) { throw "FFmpeg checksum mismatch (expected $expected, got $actual)." }
            Write-Step 'FFmpeg checksum verified'
            Expand-Archive -Path $zip -DestinationPath $tmp -Force
            $inner = Get-ChildItem -Path $tmp -Directory | Where-Object { Test-Path (Join-Path $_.FullName 'bin\ffmpeg.exe') } | Select-Object -First 1
            if (-not $inner) { throw 'Unexpected FFmpeg archive layout.' }
            if (Test-Path $ffDir) { Remove-Item -Recurse -Force $ffDir }
            Copy-Item $inner.FullName $ffDir -Recurse  # Move-Item can't cross drives in PowerShell 5.1
        } finally {
            Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
        }
    }
}
Write-Step "FFmpeg: $ffmpeg"

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
