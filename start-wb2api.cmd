@echo off
rem ============================================================
rem  WorkBuddy 网关 · 一键启动/停止（单图标）
rem  未运行 -> 启动并打开面板；正在运行 -> 5 秒内按 S 停止，否则打开面板
rem ============================================================
setlocal EnableExtensions
chcp 936 >nul 2>&1
set "ROOT=%~dp0"
set "PS=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"

if not exist "%ROOT%bin\wb2api.exe" (
    echo [x] 找不到 bin\wb2api.exe，请先编译：
    echo     go build -o bin\wb2api.exe .\cmd\server
    pause
    exit /b 1
)

title WorkBuddy 网关
tasklist /fi "imagename eq wb2api.exe" 2>nul | find /i "wb2api.exe" >nul
if errorlevel 1 goto :start

:running
echo.
echo   WorkBuddy 网关正在运行。
echo   5 秒内按 S 停止服务；不按，或按其他键 = 直接打开面板。
choice /c sn /t 5 /d n /n >nul 2>&1
if errorlevel 2 goto :panel
if errorlevel 1 goto :stop
goto :panel

:start
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%ROOT%wb2api.ps1" start
if errorlevel 1 goto :failed
goto :panel

:stop
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%ROOT%wb2api.ps1" stop
ping -n 3 127.0.0.1 >nul 2>&1
exit /b 0

:panel
rem 用 explorer 交接给已运行的资源管理器：不继承本窗口的管道
explorer "http://127.0.0.1:7863/panel/" >nul 2>&1
echo.
echo   面板： http://127.0.0.1:7863/panel/
echo   本窗口 4 秒后自动关闭。
ping -n 5 127.0.0.1 >nul 2>&1
exit /b 0

:failed
echo.
echo   [x] 启动失败，详见 logs\wb2api.log
pause
exit /b 1
