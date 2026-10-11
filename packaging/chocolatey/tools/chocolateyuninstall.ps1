$ErrorActionPreference = 'Stop'

$exe = Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Definition) 'volume_switch.exe'

$shortcut = Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Volume Switch.lnk'
Remove-Item -Path $shortcut -Force -ErrorAction SilentlyContinue

# Remove the autostart entry ("Run at Windows sign-in" in the tray menu), but
# only if it points to this package's exe.
$run = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$value = (Get-ItemProperty -Path $run -Name 'VolumeSwitch' -ErrorAction SilentlyContinue).VolumeSwitch
if ($value -eq "`"$exe`"") {
  Remove-ItemProperty -Path $run -Name 'VolumeSwitch'
}
