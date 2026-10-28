# UPAX ZKTeco relay installer for Windows.
# Run in PowerShell opened with "Run as administrator":
#
#   irm https://<host>/install.ps1 | iex
#
# Environment overrides:
#   UPAX_RELAY_BASE_URL  where release archives are downloaded from
#   UPAX_RELAY_SKIP_SETUP=1  only install the binary

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$BaseUrl = if ($env:UPAX_RELAY_BASE_URL) { $env:UPAX_RELAY_BASE_URL } else { 'https://github.com/UPAX-platform/zk-relay/releases/latest/download' }
$InstallDir = Join-Path $env:ProgramFiles 'UPAX Relay'

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host 'Please re-run this from PowerShell opened with "Run as administrator".' -ForegroundColor Red
    return
}

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$name = "upax-relay_windows_$arch"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    Write-Host "Downloading $name ..."
    Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/$name.zip" -OutFile "$tmp\relay.zip"
    Expand-Archive -Path "$tmp\relay.zip" -DestinationPath $tmp -Force

    $exe = Join-Path $InstallDir 'upax-relay.exe'
    $running = $false
    if (Test-Path $exe) {
        $running = (& $exe status 2>$null) -match 'service: running'
        if ($running) { & $exe stop | Out-Null }
    }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item "$tmp\$name\upax-relay.exe" $exe -Force

    $path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (($path -split ';') -notcontains $InstallDir) {
        [Environment]::SetEnvironmentVariable('Path', "$path;$InstallDir", 'Machine')
        $env:Path += ";$InstallDir"
    }
    Write-Host "Installed $exe ($(& $exe version))"

    if ($running) {
        & $exe start | Out-Null
        Write-Host 'Relay upgraded and restarted.'
    } elseif ($env:UPAX_RELAY_SKIP_SETUP -ne '1') {
        & $exe setup
    } else {
        Write-Host "Run 'upax-relay setup' to configure it."
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
