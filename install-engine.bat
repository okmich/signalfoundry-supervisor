@echo off
REM install-engine.bat — PROD install of the supervisor engine as a logon-launched task.
REM
REM WHY a Scheduled Task at LOGON (and NOT a Windows service):
REM   Per FLEET_SUPERVISOR_SPEC.md sec.13 + sec.9, the engine must run in the INTERACTIVE
REM   desktop session, because:
REM     - MT5 is a GUI app and needs the interactive session; a Session-0 service fights it.
REM     - The stop path borrows a child's console (AttachConsole -> Ctrl+C). A Session-0
REM       service child has no console, so the only stop is a hard-kill (orphan risk).
REM   Therefore: run-at-logon, in-session, headless. Do NOT convert this to `sc create`.
REM
REM Run this script ONCE, as Administrator, on the prod trader box. Creating a scheduled
REM task requires elevation; the task itself runs as the (non-elevated) interactive user.

setlocal
cd /d "%~dp0"

REM ---- EDIT THESE VALUES ----
set "TASK_NAME=OkmichSupervisorEngine"
REM Account the engine runs under = the dedicated interactive trading user (autologon user).
REM Defaults to the current user; override for the prod box if you install while elevated
REM as a different admin account.
set "RUN_AS=%USERDOMAIN%\%USERNAME%"
set "ENGINE_EXE=%~dp0supervisor.exe"
REM ---------------------------

REM Require Administrator (task creation needs it).
net session >nul 2>&1
if %errorLevel% neq 0 (
    echo ERROR: Please run this script as Administrator.
    pause
    exit /b 1
)

REM The binary must be built first (run build.bat).
if not exist "%ENGINE_EXE%" (
    echo ERROR: %ENGINE_EXE% not found. Build it first with build.bat.
    pause
    exit /b 1
)

echo Installing scheduled task "%TASK_NAME%"
echo   Trigger : at logon of %RUN_AS%
echo   Action  : "%ENGINE_EXE%" engine
echo   Session : interactive (/IT)
echo.

REM /SC ONLOGON  -> fire when the trading user logs on (autologon brings the desktop up).
REM /IT          -> run with an interactive token, in the user's desktop session (required
REM                 for the console-borrow stop mechanism + MT5 coexistence).
REM /RL LIMITED  -> non-elevated, matching how broker terminals run.
REM /F           -> overwrite an existing task of the same name (idempotent re-install).
schtasks /Create ^
    /TN "%TASK_NAME%" ^
    /TR "\"%ENGINE_EXE%\" engine" ^
    /SC ONLOGON ^
    /RU "%RUN_AS%" ^
    /IT ^
    /RL LIMITED ^
    /F
if errorlevel 1 goto :fail

echo.
echo Installed. The engine will start at next logon of %RUN_AS%.
echo To start it now without logging off:   schtasks /Run /TN "%TASK_NAME%"
echo To remove it:                          schtasks /Delete /TN "%TASK_NAME%" /F
echo To inspect it:                         schtasks /Query  /TN "%TASK_NAME%" /V /FO LIST
endlocal
exit /b 0

:fail
echo.
echo INSTALL FAILED.
endlocal
exit /b 1
