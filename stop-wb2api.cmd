@echo off
rem  stop-wb2api.cmd - one-click stop for the WorkBuddy gateway
setlocal
set "ROOT=%~dp0"
set "PS=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"

title WorkBuddy Gateway - stop
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%ROOT%wb2api.ps1" stop
ping -n 4 127.0.0.1 >nul 2>&1
exit /b 0
