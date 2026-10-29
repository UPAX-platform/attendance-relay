# End-to-end check of the relay on a real Windows machine, against a fake
# UPAX and a simulated terminal (cmd/relaycheck). Run from an elevated
# PowerShell at the repository root; it installs, exercises and uninstalls
# the real service and creates (then deletes) a standard local user.
#   pwsh scripts/windows-check.ps1 [-Work <dir>]
param([string]$Work = (Join-Path ([IO.Path]::GetTempPath()) 'relaycheck'))

$ErrorActionPreference = 'Stop'
$failures = [Collections.Generic.List[string]]::new()
$dataDir = Join-Path $env:ProgramData 'UPAX Relay'
$keyPath = Join-Path $dataDir 'relay.key'
$cfgPath = Join-Path $dataDir 'config.yaml'
$installed = Join-Path $env:ProgramFiles 'UPAX Relay\upax-relay.exe'
$userName = 'relaycheck'
$SYSTEM, $ADMINS, $USERS = 'S-1-5-18', 'S-1-5-32-544', 'S-1-5-32-545'

function Step($name) { Write-Host "`n=== $name" -ForegroundColor Cyan }
function Check($name, [bool]$ok, $detail = '') {
    if ($ok) { Write-Host "PASS  $name" -ForegroundColor Green }
    else { Write-Host "FAIL  $name $detail" -ForegroundColor Red; $failures.Add($name) }
}
# Runs upax-relay with an argument list (an array, so --flags reach it as is).
function Relay([string]$Exe, [string[]]$A) {
    Write-Host "> upax-relay $($A -join ' ')"
    & $Exe @A | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "upax-relay $($A -join ' ') exited $LASTEXITCODE" }
}
function Build([string]$Out, [string]$Pkg, [string]$Version = '') {
    New-Item -ItemType Directory -Force (Split-Path $Out) | Out-Null
    if ($Version) { go build -o $Out -ldflags "-X main.version=$Version" $Pkg } else { go build -o $Out $Pkg }
    if ($LASTEXITCODE -ne 0) { throw "go build $Pkg failed" }
}
function SameSet($a, $b) { (@($a | Sort-Object -Unique) -join ',') -eq (@($b | Sort-Object -Unique) -join ',') }
function State { Invoke-RestMethod "$($s.control)/state" }
function WaitFor($what, [scriptblock]$cond, [int]$secs = 90) {
    $deadline = (Get-Date).AddSeconds($secs)
    while ((Get-Date) -lt $deadline) {
        if (& $cond) { Check $what $true; return }
        Start-Sleep -Milliseconds 500
    }
    Check $what $false "(timed out after ${secs}s)"
}
function Punch($pin) { Invoke-RestMethod -Method Post "$($s.control)/punch?pin=$pin" | Out-Null }
function Sids($acl) {
    $acl.Access | ForEach-Object { $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value }
}
# A secret file: own ACL (not inherited), SYSTEM and Administrators only.
function CheckPrivate($path) {
    $acl = Get-Acl $path
    $sids = @(Sids $acl | Sort-Object -Unique)
    Check "$(Split-Path $path -Leaf): ACL protected from inheritance" $acl.AreAccessRulesProtected
    Check "$(Split-Path $path -Leaf): only SYSTEM and Administrators" (SameSet $sids @($ADMINS, $SYSTEM)) "(has $($sids -join ', '))"
}
function ServiceState { (Get-Service -Name 'upax-relay' -ErrorAction SilentlyContinue).Status }

New-Item -ItemType Directory -Force $Work | Out-Null
$logs = Join-Path $Work 'logs'
New-Item -ItemType Directory -Force $logs | Out-Null
$helper = $null
try {
    Step 'build'
    Build "$Work\upax-relay.exe" ./cmd/upax-relay check-1
    Build "$Work\v2\upax-relay.exe" ./cmd/upax-relay check-2
    Build "$Work\relaycheck.exe" ./cmd/relaycheck

    $statePath = Join-Path $Work 'state.json'
    Remove-Item $statePath -ErrorAction SilentlyContinue
    $helper = Start-Process "$Work\relaycheck.exe" -ArgumentList '-state', "`"$statePath`"" -PassThru -NoNewWindow `
        -RedirectStandardOutput "$logs\relaycheck.out.log" -RedirectStandardError "$logs\relaycheck.err.log"
    $deadline = (Get-Date).AddSeconds(30)
    while (-not (Test-Path $statePath) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 200 }
    $s = Get-Content -Raw $statePath | ConvertFrom-Json
    $expect = [int]$s.seeded

    Step 'setup: pair, add the terminal, install and start the service'
    Relay "$Work\upax-relay.exe" @('setup', '--upax', $s.upax, '--code', $s.codes[0], '--terminal', $s.terminal,
        '--backfill', 'all', '--listener', 'no', '--install', 'yes')
    Check 'service installed and running' ((ServiceState) -eq 'Running')
    Check 'binary installed under Program Files' (Test-Path $installed)
    WaitFor "stored punches uploaded ($expect)" { (State).punches -ge $expect }

    Step 'file permissions'
    CheckPrivate $keyPath
    CheckPrivate $cfgPath
    $dirAcl = Get-Acl $dataDir
    Check 'data folder: ACL protected from inheritance' $dirAcl.AreAccessRulesProtected
    $writers = $dirAcl.Access | Where-Object {
        $_.AccessControlType -eq 'Allow' -and
        ($_.FileSystemRights -band ([Security.AccessControl.FileSystemRights]'Write, CreateFiles, AppendData, Delete, ChangePermissions, TakeOwnership')) -ne 0
    } | ForEach-Object { $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value } | Sort-Object -Unique
    Check 'data folder: only SYSTEM and Administrators can write' (SameSet $writers @($ADMINS, $SYSTEM)) "(writers: $($writers -join ', '))"
    Check 'data folder: Users can read' ((Sids $dirAcl) -contains $USERS)
    $leftover = @(Get-ChildItem $dataDir -Filter '*.tmp')
    Check 'no temporary files left' ($leftover.Count -eq 0) "($($leftover.Name -join ', '))"

    Step 'a standard (non-admin) user'
    $pw = 'Rc!' + [guid]::NewGuid().ToString('N').Substring(0, 20) + 'aA1'
    net user $userName $pw /add /y | Out-Null
    $probe = Join-Path $env:PUBLIC 'relaycheck-probe.ps1'
    $probeOut = Join-Path $env:PUBLIC 'relaycheck-probe.json'
    Remove-Item $probeOut -ErrorAction SilentlyContinue
    @"
`$r = [ordered]@{}
function Probe(`$name, [scriptblock]`$b) { try { & `$b | Out-Null; `$r[`$name] = 'allowed' } catch { `$r[`$name] = 'denied' } }
Probe read_key    { Get-Content -Raw '$keyPath' -ErrorAction Stop }
Probe read_config { Get-Content -Raw '$cfgPath' -ErrorAction Stop }
Probe read_health { Get-Content -Raw '$dataDir\health-relay.json' -ErrorAction Stop }
Probe create_file { Set-Content '$dataDir\planted.txt' 'x' -ErrorAction Stop }
Probe plant_tmp   { Set-Content '$keyPath.tmp' 'x' -ErrorAction Stop }
`$r.status = (& '$installed' status 2>&1 | Out-String)
`$r.status_exit = `$LASTEXITCODE
`$r | ConvertTo-Json | Set-Content '$probeOut'
"@ | Set-Content $probe
    $cred = [pscredential]::new($userName, (ConvertTo-SecureString $pw -AsPlainText -Force))
    Start-Process powershell.exe -Credential $cred -Wait -WorkingDirectory $env:PUBLIC `
        -ArgumentList '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$probe`""
    $p = Get-Content -Raw $probeOut | ConvertFrom-Json
    Check 'user cannot read relay.key' ($p.read_key -eq 'denied')
    Check 'user cannot read config.yaml' ($p.read_config -eq 'denied')
    Check 'user cannot create files in the data folder' ($p.create_file -eq 'denied')
    Check 'user cannot plant a file next to the key' ($p.plant_tmp -eq 'denied')
    Check 'user can read the health files' ($p.read_health -eq 'allowed')
    Check 'user can run status' ($p.status_exit -eq 0 -and $p.status -match 'uploaded') "(exit $($p.status_exit))"
    $p.status | Out-File "$logs\status-as-user.txt"

    Step 'rotate-key while the service runs'
    $before = (Get-FileHash $keyPath).Hash
    Relay $installed @('rotate-key')
    Check 'key replaced' ((Get-FileHash $keyPath).Hash -ne $before)
    Check 'old key kept as relay.key.prev' (Test-Path "$keyPath.prev")
    Check 'no pending key left' (-not (Test-Path "$keyPath.next"))
    CheckPrivate $keyPath
    CheckPrivate "$keyPath.prev"
    Invoke-RestMethod -Method Post "$($s.control)/drop-prev" | Out-Null # only the new key works now
    Punch 9001; $expect++
    WaitFor 'service signs with the rotated key' { (State).punches -ge $expect }

    Step 'pair again with a new key while the service runs'
    $before = (Get-FileHash $keyPath).Hash
    Relay $installed @('pair', '--code', $s.codes[1], '--new-key')
    Check 'key replaced' ((Get-FileHash $keyPath).Hash -ne $before)
    CheckPrivate $keyPath
    Punch 9002; $expect++
    WaitFor 'service works after re-pairing' { (State).punches -ge $expect }

    Step 'upgrade: setup from a new build over the running service'
    Check 'service running before upgrade' ((ServiceState) -eq 'Running')
    Relay "$Work\v2\upax-relay.exe" @('setup', '--listener', 'no', '--install', 'yes')
    $v = (& $installed version | Out-String)
    Check 'installed binary replaced' ($v -match 'check-2') "($($v.Trim()))"
    Check 'service running after upgrade' ((ServiceState) -eq 'Running')
    Check 'no staged binary left' (-not (Test-Path "$installed.new"))
    Punch 9003; $expect++
    WaitFor 'upgraded service uploads' { (State).punches -ge $expect }

    Step 'uninstall'
    Relay $installed @('stop')
    Relay $installed @('uninstall')
    Check 'service removed' ($null -eq (ServiceState))
}
catch {
    Check 'script ran to the end' $false "($_)"
}
finally {
    Copy-Item "$dataDir\relay.log" $logs -ErrorAction SilentlyContinue
    Copy-Item "$dataDir\health-*.json" $logs -ErrorAction SilentlyContinue
    if (ServiceState) { & sc.exe stop upax-relay | Out-Null; & sc.exe delete upax-relay | Out-Null }
    net user $userName /delete 2>$null | Out-Null
    if ($helper) { Stop-Process -Id $helper.Id -ErrorAction SilentlyContinue }
}

Write-Host ''
if ($failures.Count -gt 0) {
    Write-Host "$($failures.Count) check(s) failed:" -ForegroundColor Red
    $failures | ForEach-Object { Write-Host "  $_" }
    exit 1
}
Write-Host 'All Windows checks passed.' -ForegroundColor Green
