# set-okmich-env.ps1
# Sets system-level (machine-wide) environment variables for ALL users.
# Must be run as Administrator.

#requires -RunAsAdministrator

# ---- EDIT THESE VALUES ----
$vars = @{
    "OKMICH_QUANT_GLOBAL_CONFIG"        = "E:\quant\.global"
    "OKMICH_QUANT_LIVE_BASE"            = "E:\quant\live"
    "OKMICH_QUANT_LOG_BASE"             = "E:\quant\logs"
    "OKMICH_QUANT_ENV_DIR"              = "E:\quant\env"
    "OKMICH_QUANT_SUPERVISOR_STATE_DIR" = "E:\quant\supervisor_state"
    "OKMICH_QUANT_PYTHON"               = "E:\project\quant-studies\signalfoundry\.venv\Scripts\python.exe"
}
# ---------------------------

foreach ($name in $vars.Keys) {
    $value = $vars[$name]
    [Environment]::SetEnvironmentVariable($name, $value, "Machine")
    Write-Host "Set $name = $value" -ForegroundColor Green
}

Write-Host "`nDone. All variables set at system (Machine) level." -ForegroundColor Cyan
Write-Host "Note: Open a NEW terminal/session for the changes to be visible." -ForegroundColor Yellow