# Keeps one process running on Windows: restarts it when it exits, or when its
# /health fails three checks in a row, with backoff; keeps a copy of its log
# from each crash; and writes restart and model-load metrics for the gateway
# to serve at /metrics/files ("metrics_files" in gateway.json).
#
#   .\supervise.ps1 -Name desktop-6900xt -Worker                # llama-server from worker.ps1's saved config
#   .\supervise.ps1 -Name rdnagw -Exe cmd.exe -Arguments '/c ""C:\Users\lab\rdnagw\rdnagw.exe" -config "C:\Users\lab\rdnagw\gateway.json" >> "C:\Users\lab\rdnagw\gateway.log" 2>&1"' `
#     -Health http://127.0.0.1:8080/health -Log C:\Users\lab\rdnagw\gateway.log
#   .\supervise.ps1 -Name desktop-6900xt -Worker -RegisterTask  # run it at logon (replaces worker.ps1 -RegisterTask)
#
# Metrics, in $Root\metrics\supervisor-<Name>.prom (the same names hwmetrics
# writes for systemd units on Linux):
#   worker_restarts_total{worker, reason="exit|health"}  restarts since the supervisor started
#   worker_last_restart_timestamp_seconds{worker}        when the running process was started
#   worker_model_load_seconds{worker}                    start to first healthy /health, last load
#   worker_model_info{worker, model, ctx}                the loaded model's alias and per-slot context
# A crash copy's file name goes in the supervisor log only, never in a metric.
param(
  [Parameter(Mandatory)][ValidatePattern('^[A-Za-z0-9._-]+$')][string]$Name,
  [switch]$Worker,
  [string]$Exe,
  [string]$Arguments = '',
  [string]$Health,
  [string]$Log,            # the process's own log, copied aside when it crashes
  [switch]$Props,          # read the model name and context from /props (llama-server)
  [int]$Failures = 3,      # consecutive failed health checks before a restart
  [int]$IntervalSec = 10,
  [int]$LoadTimeoutSec = 900,
  [int]$KeepCrashCopies = 20,
  [string]$Root = "$env:USERPROFILE\llama",
  [string]$MetricsDir = "$Root\metrics",
  [switch]$RegisterTask
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

if ($RegisterTask) {
  $script = $MyInvocation.MyCommand.Path
  $pass = ($PSBoundParameters.GetEnumerator() | Where-Object Key -ne 'RegisterTask' | ForEach-Object {
      if ($_.Value -is [switch]) { "-$($_.Key)" } else { "-$($_.Key) `"$($_.Value -replace '"', '\"')`"" }
    }) -join ' '
  $action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$script`" $pass" -WorkingDirectory $Root
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
  $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -RestartCount 99 -RestartInterval (New-TimeSpan -Minutes 1) `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
  Register-ScheduledTask -TaskName "rdna-supervise-$Name" -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
  Write-Host "registered logon task 'rdna-supervise-$Name'"
  return
}

if ($Worker) {
  $cmd = & (Join-Path $PSScriptRoot 'worker.ps1') -PrintCommand -Root $Root | ConvertFrom-Json
  $Exe, $Arguments, $Log = $cmd.exe, $cmd.args, $cmd.log
  if (-not $Health) { $Health = "http://127.0.0.1:$($cmd.port)/health" }
  $Props = $true
}
if (-not $Exe) { throw 'pass -Worker or -Exe' }

New-Item -ItemType Directory -Force $Root, $MetricsDir, "$Root\crash" | Out-Null
$supLog = Join-Path $Root "supervise-$Name.log"
$metricsFile = Join-Path $MetricsDir "supervisor-$Name.prom"

function Write-Log([string]$msg) {
  "$((Get-Date).ToString('yyyy-MM-ddTHH:mm:ssK')) $msg" | Add-Content $supLog
}

# Prometheus label value: escape backslashes, quotes and newlines.
function Q([string]$v) { '"' + ($v -replace '\\', '\\' -replace '"', '\"' -replace "`n", '\n') + '"' }

$restarts = @{ exit = 0; health = 0 }
$started = $null      # unix seconds of the running process's start
$loadSeconds = $null
$model = $null; $ctx = $null

function Write-Metrics {
  $w = Q $Name
  $lines = @(
    '# HELP worker_restarts_total Worker restarts by its supervisor, by reason.'
    '# TYPE worker_restarts_total counter'
    "worker_restarts_total{worker=$w,reason=`"exit`"} $($restarts.exit)"
    "worker_restarts_total{worker=$w,reason=`"health`"} $($restarts.health)"
    "# HELP worker_last_restart_timestamp_seconds When the worker's running process was (re)started."
    '# TYPE worker_last_restart_timestamp_seconds gauge'
  )
  if ($started) { $lines += "worker_last_restart_timestamp_seconds{worker=$w} $started" }
  $lines += '# HELP worker_model_load_seconds Time from process start to the first healthy /health, for the last load.'
  $lines += '# TYPE worker_model_load_seconds gauge'
  if ($null -ne $loadSeconds) { $lines += "worker_model_load_seconds{worker=$w} $($loadSeconds.ToString('0.000', [Globalization.CultureInfo]::InvariantCulture))" }
  $lines += "# HELP worker_model_info The loaded model's name and per-slot context size."
  $lines += '# TYPE worker_model_info gauge'
  if ($model) { $lines += "worker_model_info{worker=$w,model=$(Q $model),ctx=$(Q "$ctx")} 1" }
  # Write then rename, so the gateway never serves half a file.
  $tmp = "$metricsFile.tmp"
  [IO.File]::WriteAllText($tmp, (($lines -join "`n") + "`n"))
  Move-Item -Force $tmp $metricsFile
}

function Test-Health {
  if (-not $Health) { return $true }
  try { Invoke-RestMethod $Health -TimeoutSec 3 | Out-Null; return $true } catch { return $false }
}

function Read-Props {
  try {
    $p = Invoke-RestMethod ($Health -replace '/health$', '/props') -TimeoutSec 5
    $m = if ($p.model_alias) { $p.model_alias } else { Split-Path -Leaf $p.model_path }
    return @($m, $p.default_generation_settings.n_ctx)
  } catch { return @($null, $null) }
}

# Kill the process and its children (cmd.exe wrappers), then wait for it to
# be gone: a llama-server that is still freeing VRAM makes the next one fail.
function Stop-Tree($proc) {
  if ($proc.HasExited) { return }
  if ($env:OS -eq 'Windows_NT') { & taskkill.exe /T /F /PID $proc.Id 2>&1 | Out-Null } else { $proc.Kill() }
  if (-not $proc.WaitForExit(180000)) { Write-Log "pid $($proc.Id) did not exit within 3 minutes" }
  Start-Sleep 2
}

# Copy the process's log aside before the next start overwrites it.
function Save-CrashCopy([string]$reason) {
  if (-not $Log -or -not (Test-Path $Log)) { return $null }
  $file = "$Name-$((Get-Date).ToString('yyyyMMdd-HHmmss'))-$reason.log"
  Copy-Item $Log (Join-Path "$Root\crash" $file)
  Get-ChildItem "$Root\crash\$Name-*.log" | Sort-Object LastWriteTime -Descending |
    Select-Object -Skip $KeepCrashCopies | Remove-Item -Force
  return $file
}

$backoff = 5
Write-Log "supervising $Name"
Write-Metrics
while ($true) {
  $sp = @{ FilePath = $Exe; WorkingDirectory = $Root; PassThru = $true }
  if ($env:OS -eq 'Windows_NT') { $sp.WindowStyle = 'Hidden' }
  if ($Arguments) { $sp.ArgumentList = $Arguments }
  $p = Start-Process @sp
  $null = $p.Handle # keeps ExitCode readable after the process exits
  $t0 = Get-Date
  $started = [DateTimeOffset]::new($t0).ToUnixTimeSeconds()
  $model = $null; $ctx = $null
  Write-Log "started pid $($p.Id)"
  Write-Metrics

  $loaded = $false; $fails = 0; $reason = 'exit'
  while (-not $p.HasExited) {
    Start-Sleep -Seconds $(if ($loaded) { $IntervalSec } else { 1 })
    if ($p.HasExited) { break }
    $ok = Test-Health
    if ($ok -and -not $loaded) {
      $loaded = $true
      if ($Health) { $loadSeconds = ((Get-Date) - $t0).TotalSeconds }
      if ($Props) { $model, $ctx = Read-Props }
      Write-Log ("healthy after {0:0.0}s{1}" -f ((Get-Date) - $t0).TotalSeconds, $(if ($model) { " ($model)" }))
      Write-Metrics
    } elseif ($ok) {
      $fails = 0
    } elseif ($loaded) {
      $fails++
      if ($fails -ge $Failures) { $reason = 'health'; Write-Log "$fails failed health checks; restarting"; Stop-Tree $p; break }
    } elseif (((Get-Date) - $t0).TotalSeconds -gt $LoadTimeoutSec) {
      $reason = 'health'; Write-Log "not healthy after ${LoadTimeoutSec}s; restarting"; Stop-Tree $p; break
    }
  }

  $code = if ($p.HasExited) { $p.ExitCode } else { 'n/a' }
  $copy = Save-CrashCopy $reason
  $restarts[$reason]++
  $model = $null; $ctx = $null
  $ran = ((Get-Date) - $t0).TotalSeconds
  if ($ran -ge 600) { $backoff = 5 }
  Write-Log ("exited ({0}, code {1}) after {2:0}s; crash copy {3}; restarting in {4}s" -f $reason, $code, $ran, $(if ($copy) { $copy } else { 'none' }), $backoff)
  Write-Metrics
  Start-Sleep -Seconds $backoff
  $backoff = [Math]::Min($backoff * 2, 300)
}
