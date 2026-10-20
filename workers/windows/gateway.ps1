# Runs rdnagw on a Windows box as a per-user logon task (no admin needed).
#
#   .\gateway.ps1 -Install -Exe .\rdnagw.exe -Config .\gateway.json
#   .\gateway.ps1 -Start / -Stop / -Status / -Uninstall
param(
  [switch]$Install,
  [switch]$Uninstall,
  [switch]$Start,
  [switch]$Stop,
  [switch]$Status,
  [string]$Exe,
  [string]$Config,
  [string]$Root = "$env:USERPROFILE\rdnagw"
)
$ErrorActionPreference = 'Stop'
$task = 'rdna-gateway'

function Stop-Gateway { Get-Process rdnagw -ErrorAction SilentlyContinue | Stop-Process -Force }

function Start-Gateway {
  Stop-Gateway
  # Win32_Process.Create escapes the SSH session's job object.
  $cmd = "cmd /c `"`"$Root\rdnagw.exe`" -config `"$Root\gateway.json`" >> `"$Root\gateway.log`" 2>&1`""
  $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cmd; CurrentDirectory = $Root }
  if ($r.ReturnValue -ne 0) { throw "launch failed: $($r.ReturnValue)" }
}

function Show-Status {
  $c = Get-Content -Raw "$Root\gateway.json" | ConvertFrom-Json
  $hostPart, $port = $c.listen -split ':', 2
  if (-not $hostPart -or $hostPart -eq '0.0.0.0') { $hostPart = '127.0.0.1' }
  for ($i = 0; $i -lt 15; $i++) {
    try {
      $h = Invoke-RestMethod "http://${hostPart}:$port/health" -TimeoutSec 2
      Write-Host "gateway up on :$port, workers $($h.workers_up)/$($h.workers)"; return
    } catch { Start-Sleep 1 }
  }
  Write-Host "gateway down (see $Root\gateway.log)"; exit 1
}

if ($Install) {
  New-Item -ItemType Directory -Force $Root | Out-Null
  Stop-Gateway
  $dstExe = Join-Path $Root 'rdnagw.exe'
  if ((Resolve-Path $Exe).Path -ne $dstExe) { Copy-Item -Force $Exe $dstExe }
  if ($Config) { Copy-Item -Force $Config (Join-Path $Root 'gateway.json') }
  $action = New-ScheduledTaskAction -Execute 'cmd.exe' `
    -Argument "/c `"`"$Root\rdnagw.exe`" -config `"$Root\gateway.json`" >> `"$Root\gateway.log`" 2>&1`"" -WorkingDirectory $Root
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
  $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -RestartCount 99 -RestartInterval (New-TimeSpan -Minutes 1) `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
  Register-ScheduledTask -TaskName $task -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
  Write-Host "registered logon task '$task'"
  Start-Gateway; Show-Status; return
}
if ($Uninstall) { Stop-Gateway; Unregister-ScheduledTask -TaskName $task -Confirm:$false; Write-Host "removed"; return }
if ($Stop) { Stop-Gateway; return }
if ($Start) { Start-Gateway; Show-Status; return }
if ($Status) { Show-Status }
