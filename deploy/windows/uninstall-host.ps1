<#
.SYNOPSIS
  Removes the KloudIT Recon host agent (task, firewall rule and files). A virtual display a
  stream left (the agent is stopped without its cleanup) is removed and the display layout
  restored first.
.PARAMETER KeepConfig
  Keep %APPDATA%\KlouditRecon (pairing and settings).
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

Stop-ScheduledTask -TaskName 'KloudIT Recon Host' -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName 'KloudIT Recon Host' -Confirm:$false -ErrorAction SilentlyContinue
Get-NetFirewallRule -DisplayName 'KloudIT Recon host (direct path)' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
Get-Process -Name 'recon-hostw', 'recon-host' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Get-Process -Name 'ffmpeg', 'recon-encoder' -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$InstallDir\*" } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 800
# Killed like this, the agent could not remove a virtual display of a session (or one kept
# for a reconnect) and put the display layout back (with "virtualDisplayLayout": "only" the
# monitors stay off): do it now from its restore journal, next to the config, before the
# program and the journal are deleted.
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
    if (Test-Path $cfgDir) { Remove-Item -Recurse -Force $cfgDir }
}
Write-Host 'KloudIT Recon host agent removed.' -ForegroundColor Green
