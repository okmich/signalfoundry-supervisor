@echo off
REM run-engine.bat — DEV: run the supervisor engine in this terminal (blocking, foreground).
REM
REM This is the dev counterpart to install-engine.bat (which installs the prod logon task).
REM The engine is a long-running, singleton process: it holds this console, reconciles state,
REM and stays up until you press Ctrl+C (which triggers a graceful shutdown).
REM
REM Open a SECOND terminal and run:  supervisor.exe tui   (the dashboard).

setlocal
cd /d "%~dp0"

set "ENGINE_EXE=%~dp0supervisor.exe"

if not exist "%ENGINE_EXE%" (
    echo ERROR: %ENGINE_EXE% not found. Build it first with build.bat.
    pause
    exit /b 1
)

echo Starting engine in foreground. Press Ctrl+C to stop.
echo (Open another terminal and run: supervisor.exe tui)
echo.
"%ENGINE_EXE%" engine

endlocal
exit /b %errorlevel%
