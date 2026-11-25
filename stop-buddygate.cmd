@echo off
rem  stop-buddygate.cmd - one-click stop for the WorkBuddy gateway
setlocal
set "ROOT=%~dp0"
set "PS=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"

title BuddyGate - stop
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%ROOT%buddygate.ps1" stop
ping -n 4 127.0.0.1 >nul 2>&1
exit /b 0
