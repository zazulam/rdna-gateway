# rdna-gateway worker for Windows + RDNA2 (gfx1030/1031/1032).
#
# Installs/updates a llama.cpp ROCm build for gfx103X and runs llama-server as
# a logon task, serving one GGUF. Ollama's blobs can be reused directly:
#   .\worker.ps1 -Install
#   .\worker.ps1 -Model (.\worker.ps1 -OllamaBlob gemma4:12b-it-qat) -Alias gemma4:12b
#   .\worker.ps1 -Start / -Stop / -Status
#   .\worker.ps1 -Start -Parallel 2 -Ctx 65536 -Kv q8_0 -Extra '--cache-ram 16384'
#     (settings passed with -Start are saved for next time)
#
# Why ROCm and not Vulkan: on a 6900 XT, ROCm+flash-attn processed prompts at
# ~1370 tok/s vs ~900 for Vulkan (gemma4 12B Q4_0), same ~27 tok/s decode.
# Pass -Backend vulkan for the official ggml-org build if ROCm misbehaves.
param(
  [switch]$Install,
  [switch]$Start,
  [switch]$Stop,
  [switch]$Status,
  [switch]$RegisterTask, # start the saved worker config at logon
  [string]$OllamaBlob,
  [string]$Model,
  [string]$Alias,
  [ValidateSet('rocm', 'vulkan')][string]$Backend = 'rocm',
  [int]$Port = 8081,
  [int]$Parallel = 4,
  [int]$Ctx = 131072,  # 4 slots x 32k
  [ValidateSet('f16', 'q8_0', 'q4_0')][string]$Kv = 'q8_0',
  [int]$CacheReuse = 256,
  # Extra llama-server flags, e.g. '--cache-ram 16384 --swa-full'.
  [string]$Extra = '',
  [string]$Root = "$env:USERPROFILE\llama"
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$conf = Join-Path $Root 'worker.json'

function Resolve-OllamaBlob([string]$name) {
  $n, $tag = $name -split ':', 2
  if (-not $tag) { $tag = 'latest' }
  $models = if ($env:OLLAMA_MODELS) { $env:OLLAMA_MODELS } else { "$env:USERPROFILE\.ollama\models" }
  $man = Get-Content -Raw "$models\manifests\registry.ollama.ai\library\$n\$tag" | ConvertFrom-Json
  $layer = $man.layers | Where-Object mediaType -eq 'application/vnd.ollama.image.model'
  "$models\blobs\" + ($layer.digest -replace ':', '-')
}

if ($OllamaBlob) { Resolve-OllamaBlob $OllamaBlob; return }

if ($RegisterTask) {
  $script = $MyInvocation.MyCommand.Path
  $action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$script`" -Start" -WorkingDirectory $Root
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
  $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 10) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
  Register-ScheduledTask -TaskName 'rdna-worker' -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
  Write-Host "registered logon task 'rdna-worker' (starts the saved config in $conf)"
  return
}

if ($Install) {
  New-Item -ItemType Directory -Force $Root | Out-Null
  if ($Backend -eq 'rocm') {
    $rel = Invoke-RestMethod https://api.github.com/repos/lemonade-sdk/llamacpp-rocm/releases/latest
    $asset = $rel.assets | Where-Object name -like '*windows-rocm-gfx103X-x64.zip' | Select-Object -First 1
  } else {
    $rel = Invoke-RestMethod https://api.github.com/repos/ggml-org/llama.cpp/releases/latest
    $asset = $rel.assets | Where-Object name -like '*bin-win-vulkan-x64.zip' | Select-Object -First 1
  }
  if (-not $asset) { throw "no $Backend asset in $($rel.tag_name)" }
  $zip = Join-Path $Root "$Backend.zip"
  Write-Host "downloading $($asset.name)"
  Invoke-WebRequest $asset.browser_download_url -OutFile $zip
  Expand-Archive -Force $zip (Join-Path $Root $Backend)
  Remove-Item $zip
  & (Join-Path $Root "$Backend\llama-server.exe") --list-devices
  return
}

if ($Model) {
  if (-not (Test-Path $Model)) { throw "model not found: $Model" }
  if (-not $Alias) { $Alias = [IO.Path]::GetFileNameWithoutExtension($Model) }
  @{ model = $Model; alias = $Alias; backend = $Backend; port = $Port; parallel = $Parallel; ctx = $Ctx;
     kv = $Kv; cache_reuse = $CacheReuse; extra = $Extra } |
    ConvertTo-Json | Set-Content $conf
  Write-Host "saved $conf"
} elseif ($Start) {
  # Fold any settings given alongside -Start into the saved config.
  $c = Get-Content -Raw $conf | ConvertFrom-Json
  $map = @{ Backend = 'backend'; Port = 'port'; Parallel = 'parallel'; Ctx = 'ctx'; Kv = 'kv'; CacheReuse = 'cache_reuse'; Extra = 'extra'; Alias = 'alias' }
  foreach ($k in $map.Keys) {
    if ($PSBoundParameters.ContainsKey($k)) { $c | Add-Member -Force NoteProperty $map[$k] $PSBoundParameters[$k] }
  }
  $c | ConvertTo-Json | Set-Content $conf
}

if ($Stop -or $Start) {
  # The old server can take several seconds to exit while it frees VRAM; a new
  # one launched before that loses the port/VRAM race and dies, leaving the old
  # model answering health checks. Wait until it is really gone.
  $old = Get-Process llama-server -ErrorAction SilentlyContinue
  if ($old) {
    $old | Stop-Process -Force -ErrorAction SilentlyContinue
    # An exited server can linger in the process list while the GPU driver
    # holds a handle, so test HasExited and the port rather than presence.
    $port = if (Test-Path $conf) { (Get-Content -Raw $conf | ConvertFrom-Json).port } else { $Port }
    for ($i = 0; $i -lt 180; $i++) {
      $alive = @(Get-Process llama-server -ErrorAction SilentlyContinue | Where-Object { -not $_.HasExited })
      $listening = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
      if ($alive.Count -eq 0 -and -not $listening) { break }
      Start-Sleep 1
    }
    if ($i -ge 180) { throw "llama-server did not exit" }
    Start-Sleep 2 # let the driver release VRAM
  }
}

if ($Start) {
  $c = Get-Content -Raw $conf | ConvertFrom-Json
  $kvType = if ($c.kv) { $c.kv } else { 'q8_0' }
  $reuse = if ($null -ne $c.cache_reuse) { $c.cache_reuse } else { 256 }
  $exe = Join-Path $Root "$($c.backend)\llama-server.exe"
  # -c is the total KV budget, split evenly across --parallel slots.
  # q8_0 KV halves cache VRAM vs f16 with negligible quality loss, leaving room
  # for more/longer slots on 12-16 GB cards. --cache-reuse lets a slot reuse
  # shifted chunks of a previous prompt, not just an exact prefix.
  $argv = @(
    '-m', "`"$($c.model)`"", '--alias', $c.alias,
    '-ngl', '999', '-fa', 'on', '-c', $c.ctx, '--parallel', $c.parallel,
    '-ctk', $kvType, '-ctv', $kvType, '--cache-reuse', $reuse,
    '--metrics', '--slots', '--host', '0.0.0.0', '--port', $c.port,
    '--log-file', "`"$Root\server.log`"", $c.extra
  ) -join ' '
  # Win32_Process.Create escapes the SSH session's job object, so the server
  # survives the session closing.
  $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
    CommandLine = "`"$exe`" $argv"; CurrentDirectory = $Root
  }
  if ($r.ReturnValue -ne 0) { throw "launch failed: $($r.ReturnValue)" }
  Write-Host "llama-server pid $($r.ProcessId) on :$($c.port) ($($c.alias), $($c.backend), $($c.parallel)x$([int]($c.ctx / $c.parallel)) $kvType $($c.extra))"
}

if ($Status -or $Start) {
  $c = Get-Content -Raw $conf | ConvertFrom-Json
  for ($i = 0; $i -lt 150; $i++) {
    try {
      $h = Invoke-RestMethod "http://127.0.0.1:$($c.port)/health" -TimeoutSec 2
      # Make sure it is the model we asked for, not a survivor.
      $p = Invoke-RestMethod "http://127.0.0.1:$($c.port)/props" -TimeoutSec 5
      if ($p.model_path -and $p.model_path -ne $c.model) { throw "serving $($p.model_path), expected $($c.model)" }
      Write-Host "health: $($h.status) ($($p.model_path))"; exit 0
    } catch {
      if ("$_" -like 'serving *') { Write-Host "health: wrong model: $_"; exit 1 }
      Start-Sleep 2
    }
  }
  Write-Host "health: down (see $Root\server.log)"; exit 1
}
