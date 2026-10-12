@echo off
chcp 65001 >nul
setlocal enabledelayedexpansion
cd /d "%~dp0"
title agnes-hub-go

set "BIN=agnes-hub-go.exe"
set "HOST=0.0.0.0"
set "PORT=4142"
set "DATADIR=%~dp0data"

if exist "%BIN%" goto RUN
echo [ERROR] %BIN% not found in this folder.
echo         Put agnes-hub-go.exe next to this .bat (or run `go build` first).
pause
exit /b 1

:RUN
echo ============================================================
echo   agnes-hub-go   Agnes AI multi-account relay + RPM pacing
echo ============================================================
echo Console   http://127.0.0.1:%PORT%/console
echo API base  http://127.0.0.1:%PORT%/v1
echo Model     agnes-auto   (auto routes text / image / video)
echo Data dir  %DATADIR%
echo.
echo Close this window to stop the service.
echo.

"%BIN%" -host %HOST% -port %PORT% -data "%DATADIR%"
set "CODE=%ERRORLEVEL%"
echo.
echo agnes-hub-go exited with code %CODE%.
pause
exit /b %CODE%
