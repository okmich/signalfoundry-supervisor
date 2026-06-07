@echo off
REM build.bat — clean and build the fleet supervisor (Windows native).
REM Produces supervisor.exe in the repo root.

setlocal
cd /d "%~dp0"

set "OUT=supervisor.exe"

echo === Cleaning ===
if exist "%OUT%" (
    del /f /q "%OUT%"
    echo Removed old %OUT%
)
go clean
if errorlevel 1 goto :fail

echo.
echo === Resolving dependencies ===
go mod tidy
if errorlevel 1 goto :fail

echo.
echo === Building ===
go build -o "%OUT%" ./cmd/supervisor
if errorlevel 1 goto :fail

echo.
echo BUILD OK -^> %CD%\%OUT%
endlocal
exit /b 0

:fail
echo.
echo BUILD FAILED.
endlocal
exit /b 1
