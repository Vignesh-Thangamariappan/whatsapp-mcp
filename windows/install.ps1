# Sets up the whatsapp-mcp bridge on Windows x64 without any compiler or Go toolchain.
#
# Expected layout (same as the repo):
#   <root>\whatsapp-bridge\whatsapp-bridge.exe   (built with windows/build.sh elsewhere)
#   <root>\whatsapp-mcp-server\                  (main.py, whatsapp.py, pyproject.toml, ...)
#   <root>\windows\install.ps1                   (this script)
#
# Run 1 (setup):          powershell -ExecutionPolicy Bypass -File windows\install.ps1
# Then pair once, in a visible terminal, from <root>\whatsapp-bridge:
#                         .\whatsapp-bridge.exe        (scan the QR within 3 minutes)
# Run 2 (autostart):      powershell -ExecutionPolicy Bypass -File windows\install.ps1 -RegisterTask
#
# Needs on PATH: uv, claude. Safe to run again.
param(
    [switch]$RegisterTask
)

$ErrorActionPreference = 'Stop'

$Root      = Split-Path -Parent $PSScriptRoot
$BridgeDir = Join-Path $Root 'whatsapp-bridge'
$ServerDir = Join-Path $Root 'whatsapp-mcp-server'
$Exe       = Join-Path $BridgeDir 'whatsapp-bridge.exe'
$TaskName  = 'WhatsAppBridge'

if (-not (Test-Path $Exe)) {
    throw "Missing $Exe. Build it with windows/build.sh on a machine with Go and copy it here."
}

if ($RegisterTask) {
    if (-not (Test-Path (Join-Path $BridgeDir 'store\whatsapp.db'))) {
        throw 'Not paired yet. Run whatsapp-bridge.exe in a terminal first and scan the QR code.'
    }

    # Cap the Go heap so the bridge stays small. It idles around 12 MB.
    [Environment]::SetEnvironmentVariable('GOMEMLIMIT', '64MiB', 'User')

    # Rotate the log on each start (keeps one old copy), then run hidden with output to the log.
    $cmd = "Set-Location '$BridgeDir'; " +
           "if ((Test-Path store\bridge.log) -and (Get-Item store\bridge.log).Length -gt 1MB) { Move-Item -Force store\bridge.log store\bridge.old.log }; " +
           "& '$Exe' *>> store\bridge.log"
    $action  = New-ScheduledTaskAction -Execute 'powershell.exe' `
        -Argument "-NoProfile -WindowStyle Hidden -Command `"$cmd`"" -WorkingDirectory $BridgeDir
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
        -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)

    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Registered and started scheduled task '$TaskName' (runs hidden at logon)."
    Write-Host "Check it with: netstat -ano | findstr 127.0.0.1:8080   (must show 127.0.0.1, not 0.0.0.0)"
    return
}

Write-Host '==> Checking prerequisites'
$missing = @('uv', 'claude') | Where-Object { -not (Get-Command $_ -ErrorAction SilentlyContinue) }
if ($missing) { throw "Missing on PATH: $($missing -join ', ')" }

Write-Host '==> Setting up the Python MCP server (uv fetches Python if needed)'
uv --directory $ServerDir sync
if ($LASTEXITCODE -ne 0) { throw 'uv sync failed' }

Write-Host '==> Registering the MCP server with Claude Code (user scope)'
$existing = claude mcp list 2>$null | Select-String -Pattern '^whatsapp:' -Quiet
if ($existing) {
    Write-Host "Already registered, skipping."
} else {
    claude mcp add -s user whatsapp -- uv --directory $ServerDir run main.py
    if ($LASTEXITCODE -ne 0) { throw 'claude mcp add failed' }
}

Write-Host ''
Write-Host 'Next: pair once, in a visible terminal (scan the QR within 3 minutes):'
Write-Host "  cd '$BridgeDir'"
Write-Host '  .\whatsapp-bridge.exe'
Write-Host 'On the phone: Settings > Linked Devices > Link a Device. Then Ctrl+C and run:'
Write-Host '  powershell -ExecutionPolicy Bypass -File windows\install.ps1 -RegisterTask'
