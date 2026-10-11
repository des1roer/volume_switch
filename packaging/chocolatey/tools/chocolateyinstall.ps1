$ErrorActionPreference = 'Stop'

# The URL and checksum placeholders are filled in by .github/workflows/release.yml.
$toolsDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$exe = Join-Path $toolsDir 'volume_switch.exe'

Get-ChocolateyWebFile -PackageName $env:ChocolateyPackageName `
  -FileFullPath $exe `
  -Url64bit 'https://github.com/des1roer/volume_switch/releases/download/v__VERSION__/volume_switch.exe' `
  -Checksum64 '__SHA256__' `
  -ChecksumType64 'sha256'

# A GUI tray application: the shim must not wait for it to exit.
Set-Content -Path "$exe.gui" -Value ''

Install-ChocolateyShortcut `
  -ShortcutFilePath (Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Volume Switch.lnk') `
  -TargetPath $exe
