<#
.SYNOPSIS
  Removes the KloudIT Recon host agent (task, firewall rule and files).
.PARAMETER KeepConfig
  Keep %APPDATA%\KlouditRecon (pairing and settings).
#>
[CmdletBinding()]
param(
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'KlouditRecon'),
    [switch]$KeepConfig
)
$ErrorActionPreference = 'Stop'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this script from an elevated PowerShell (Run as administrator).'
}

Unregister-ScheduledTask -TaskName 'KloudIT Recon Host' -Confirm:$false -ErrorAction SilentlyContinue
Get-NetFirewallRule -DisplayName 'KloudIT Recon host (direct path)' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
Get-Process -Name 'recon-hostw', 'recon-host' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 500
if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
if (-not $KeepConfig) {
    $cfgDir = Join-Path $env:APPDATA 'KlouditRecon'
    if (Test-Path $cfgDir) { Remove-Item -Recurse -Force $cfgDir }
}
Write-Host 'KloudIT Recon host agent removed.' -ForegroundColor Green
