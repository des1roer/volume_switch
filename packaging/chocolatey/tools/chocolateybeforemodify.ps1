$ErrorActionPreference = 'Stop'

# The running exe is locked: stop it before an upgrade or uninstall.
$exe = Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Definition) 'volume_switch.exe'
Get-Process -Name 'volume_switch' -ErrorAction SilentlyContinue |
  Where-Object { $_.Path -eq $exe } |
  Stop-Process -Force
