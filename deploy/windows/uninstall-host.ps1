<#
.SYNOPSIS
  Removes the KloudIT Recon host agent (task, firewall rule and files). A virtual display a
  stream left (the agent is stopped without its cleanup) is removed and the display layout
  restored first.
.PARAMETER KeepConfig
  Keep %APPDATA%\KlouditRecon (pairing and settings) and the agent's log in
  %ProgramData%\KlouditRecon\<user>.
.PARAMETER RemoveVirtualDisplay
  Also remove the Virtual Display Driver (its device and driver package), as installed by
  install-host.ps1 -InstallVirtualDisplay. SudoVDA (Apollo's driver) is left alone.
#>
[CmdletBinding()]
param(
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'KlouditRecon'),
    [switch]$KeepConfig,
    [switch]$RemoveVirtualDisplay
)
$ErrorActionPreference = 'Stop'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this script from an elevated PowerShell (Run as administrator).'
}

# %APPDATA%\KlouditRecon is the user's own folder: any program the user runs can put links
# (junctions, symbolic links) in it, or make it one, and turn this elevated script's recursive
# delete into a delete elsewhere. Only the files recon-host and the installer write there are
# removed, each by its name and not through a link (a link is removed itself), then the folder if
# that empties it; anything else is left, with a warning, for the user to delete.
function Remove-ConfigFolder([string]$dir) {
    $item = Get-Item -LiteralPath $dir -Force
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        [IO.Directory]::Delete($dir) # the link, not what it points at
        return
    }
    $ours = '^(host\.json|live-bitrate\.json|host\.log|host\.log\.old|vdisplay-restore\.json)(\.tmp|\.[0-9]+\.tmp|\.[0-9a-f]{32}\.tmp)?$'
    foreach ($f in @(Get-ChildItem -LiteralPath $dir -Force)) {
        if ($f.Name -notmatch $ours) { continue }
        if ($f.PSIsContainer) {
            if (($f.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { [IO.Directory]::Delete($f.FullName) }
            continue
        }
        [IO.File]::Delete($f.FullName) # a symbolic link or a hard link: only this name
    }
    if (@(Get-ChildItem -LiteralPath $dir -Force).Count -eq 0) {
        [IO.Directory]::Delete($dir)
    } else {
        Write-Warning "Left $dir, which holds files KloudIT Recon did not write there; delete it yourself from a PowerShell that is not elevated."
    }
}

Stop-ScheduledTask -TaskName 'KloudIT Recon Host' -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName 'KloudIT Recon Host' -Confirm:$false -ErrorAction SilentlyContinue
Get-NetFirewallRule -DisplayName 'KloudIT Recon host (direct path)' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
Get-Process -Name 'recon-hostw', 'recon-host' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Get-Process -Name 'ffmpeg', 'recon-encoder' -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$InstallDir\*" } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 800
# Killed like this, the agent could not remove a virtual display of a session (or one kept
# for a reconnect) and put the display layout back (with "virtualDisplayLayout": "only" the
# monitors stay off): do it now from its restore journal, in the agent's folder
# (ProgramData\KlouditRecon\<user>, which recon-host finds itself), before the program and the
# journal are deleted.
$exe = Join-Path $InstallDir 'recon-host.exe'
$cfgPath = Join-Path (Join-Path $env:APPDATA 'KlouditRecon') 'host.json'
if ((Test-Path $exe) -and (Test-Path $cfgPath)) {
    $out = & { $ErrorActionPreference = 'Continue'; & $exe -config $cfgPath vdisplay -restore 2>&1 } | ForEach-Object { "$_" } | Out-String
    if ($LASTEXITCODE -ne 0) {
        Write-Warning ("Could not put the displays back after a virtual display (recon-host exit code $LASTEXITCODE): $($out.Trim())`n" +
            'Check Settings > System > Display (Win+P), and disable the Virtual Display Driver in Device Manager > Display adapters.')
    } elseif ($out -match 'displays restored') {
        Write-Host 'Removed a virtual display a stream had left and restored the display layout.'
    }
}
if ($RemoveVirtualDisplay) {
    # pnputil /remove-device needs Windows 10 2004 or later.
    foreach ($dev in @(Get-PnpDevice -ErrorAction SilentlyContinue | Where-Object { $_.HardwareID -contains 'Root\MttVDD' })) {
        pnputil /remove-device $dev.InstanceId | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Warning "Could not remove device $($dev.InstanceId) (pnputil exit code $LASTEXITCODE)." }
    }
    foreach ($drv in @(Get-WindowsDriver -Online -ErrorAction SilentlyContinue | Where-Object { $_.OriginalFileName -like '*\mttvdd.inf' })) {
        pnputil /delete-driver $drv.Driver /uninstall /force | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Warning "Could not delete driver package $($drv.Driver) (pnputil exit code $LASTEXITCODE)." }
    }
}
if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
if (-not $KeepConfig) {
    $cfgDir = Join-Path $env:APPDATA 'KlouditRecon'
    if (Test-Path -LiteralPath $cfgDir) { Remove-ConfigFolder $cfgDir }
    $stateRoot = Join-Path ([Environment]::GetFolderPath('CommonApplicationData')) 'KlouditRecon'
    $stateDir = Join-Path $stateRoot (($identity.Name -split '\\')[-1])
    if (Test-Path -LiteralPath $stateDir) { Remove-Item -LiteralPath $stateDir -Recurse -Force }
    if ((Test-Path -LiteralPath $stateRoot) -and -not (Get-ChildItem -LiteralPath $stateRoot -Force)) {
        Remove-Item -LiteralPath $stateRoot -Force
    }
}
Write-Host 'KloudIT Recon host agent removed.' -ForegroundColor Green
