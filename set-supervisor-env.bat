@echo off
REM set-okmich-env.bat
REM Sets system-level environment variables for ALL users.
REM Must be run as Administrator.

REM ---- EDIT THESE VALUES ----
set "GLOBAL_CONFIG=E:\quant\.global"
set "LIVE_BASE=E:\quant\live"
set "LOG_BASE=E:\quant\logs"
set "ENV_DIR=E:\quant\env"
set "STATE_DIR=E:\quant\supervisor_state"
set "PYTHON=E:\project\quant-studies\signalfoundry\.venv\Scripts\python.exe"
REM ---------------------------

REM Check for admin rights
net session >nul 2>&1
if %errorLevel% neq 0 (
    echo ERROR: Please run this script as Administrator.
    pause
    exit /b 1
)

setx OKMICH_QUANT_LIVE_BASE "%LIVE_BASE%" /M
setx OKMICH_QUANT_LOG_BASE "%LOG_BASE%" /M
setx OKMICH_QUANT_ENV_DIR "%ENV_DIR%" /M
setx OKMICH_QUANT_SUPERVISOR_STATE_DIR "%STATE_DIR%" /M
setx OKMICH_QUANT_GLOBAL_CONFIG "%GLOBAL_CONFIG%" /M
setx OKMICH_QUANT_PYTHON "%PYTHON%" /M

echo.
echo Done. Open a NEW terminal for changes to take effect.
pause