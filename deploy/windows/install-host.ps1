<#
.SYNOPSIS
  Installs the KloudIT Recon host agent on this Windows PC.

.DESCRIPTION
  - Copies recon-host.exe / recon-hostw.exe to the install directory
  - Downloads FFmpeg (BtbN GPL build, SHA-256 verified) unless -FFmpegPath is given
  - Optionally pairs with your gateway (-PairingCode)
  - Registers a logon task that runs the agent hidden, with highest privileges
    (needed to send input to elevated games/launchers), restarting on failure
  - Opens the direct-path UDP port in Windows Firewall (Private/Domain only,
    scoped to the agent executable)
  - Optionally installs the ViGEmBus driver for virtual Xbox controllers

  Run from an elevated PowerShell in the folder containing the binaries:
    powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -PairingCode "recon1:..."

.PARAMETER PairingCode
  Code shown by the gateway's "Add a PC" dialog. Can also be applied later with
  recon-host.exe pair <code>.
.PARAMETER InstallDir
  Where to install (default: Program Files\KlouditRecon).
.PARAMETER FFmpegPath
  Use an existing ffmpeg.exe (FFmpeg 7.1+, 8.0+ recommended for gfxcapture).
.PARAMETER DirectPort
  UDP port for direct LAN connections from the browser (0 disables the direct path).
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
    [switch]$InstallViGEm,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$TaskName = 'KloudIT Recon Host'
$RuleName = 'KloudIT Recon host (direct path)'

function Write-Step([string]$msg) { Write-Host "==> $msg" -ForegroundColor Cyan }

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

# --- Binaries ----------------------------------------------------------------
Write-Step "Installing agent to $InstallDir"
Get-Process -Name 'recon-hostw', 'recon-host' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item (Join-Path $src 'recon-host.exe'), (Join-Path $src 'recon-hostw.exe') $InstallDir -Force
foreach ($f in 'install-host.ps1', 'uninstall-host.ps1') {
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
    if (-not (Test-Path $ffmpeg)) {
        Write-Step 'Downloading FFmpeg (GPU capture + NVENC/AMF/QSV encoders)'
        $base = 'https://github.com/BtbN/FFmpeg-Builds/releases/download/latest'
        $zipName = 'ffmpeg-master-latest-win64-gpl.zip'
        $tmp = Join-Path $env:TEMP ("recon-ffmpeg-" + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        try {
            $zip = Join-Path $tmp $zipName
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$zipName" -OutFile $zip
            $sums = (Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.sha256").Content
            if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
            $line = ($sums -split "`n") | Where-Object { $_ -match [regex]::Escape($zipName) + '\s*$' } | Select-Object -First 1
            if (-not $line) { throw 'Could not find the FFmpeg checksum.' }
            $expected = ($line -split '\s+')[0].ToLowerInvariant()
            $actual = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLowerInvariant()
            if ($expected -ne $actual) { throw "FFmpeg checksum mismatch (expected $expected, got $actual)." }
            Write-Step 'FFmpeg checksum verified'
            Expand-Archive -Path $zip -DestinationPath $tmp -Force
            $inner = Get-ChildItem -Path $tmp -Directory | Where-Object { Test-Path (Join-Path $_.FullName 'bin\ffmpeg.exe') } | Select-Object -First 1
            if (-not $inner) { throw 'Unexpected FFmpeg archive layout.' }
            if (Test-Path $ffDir) { Remove-Item -Recurse -Force $ffDir }
            Move-Item $inner.FullName $ffDir
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
    $existing = Get-Content -Raw $cfgPath | ConvertFrom-Json
    foreach ($p in $existing.PSObject.Properties) { $cfg[$p.Name] = $p.Value }
}
$cfg['ffmpeg'] = $ffmpeg
$cfg['directPort'] = $DirectPort
if (-not $cfg.Contains('audio')) { $cfg['audio'] = $true }
if (-not $cfg.Contains('gamepad')) { $cfg['gamepad'] = $true }
($cfg | ConvertTo-Json -Depth 5) | Set-Content -Encoding UTF8 -Path $cfgPath
# Owner-only access: the file holds the host token.
icacls $cfgDir /inheritance:r /grant:r "${env:USERNAME}:(OI)(CI)F" "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F" | Out-Null

$exe = Join-Path $InstallDir 'recon-host.exe'
$exeW = Join-Path $InstallDir 'recon-hostw.exe'
if ($PairingCode) {
    Write-Step 'Pairing with gateway'
    & $exe -config $cfgPath pair $PairingCode
    if ($LASTEXITCODE -ne 0) { throw 'Pairing failed.' }
}

# --- Firewall ------------------------------------------------------------------
Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
if ($DirectPort -gt 0) {
    Write-Step "Allowing inbound UDP $DirectPort for the direct path (Private/Domain networks)"
    New-NetFirewallRule -DisplayName $RuleName -Direction Inbound -Action Allow -Protocol UDP `
        -LocalPort $DirectPort -Program $exeW -Profile Private, Domain | Out-Null
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
& $exe -config $cfgPath probe

if (-not $NoStart) {
    Start-ScheduledTask -TaskName $TaskName
    Start-Sleep -Seconds 2
    $running = Get-Process -Name 'recon-hostw' -ErrorAction SilentlyContinue
    if ($running) { Write-Step 'Agent is running.' } else { Write-Warning "Agent did not start; see $logPath" }
}

Write-Host ''
Write-Host 'KloudIT Recon host agent installed.' -ForegroundColor Green
Write-Host "  Config: $cfgPath"
Write-Host "  Log:    $logPath"
if (-not $cfg.Contains('gateway') -and -not $PairingCode) {
    Write-Host "  Next:   & '$exe' -config '$cfgPath' pair <code-from-gateway>; then Start-ScheduledTask '$TaskName'"
}
Write-Host '  Tip:    for unattended use enable automatic sign-in and disable the lock screen: the agent'
Write-Host '          runs in your desktop session and cannot capture the Windows lock screen or UAC prompts.'
