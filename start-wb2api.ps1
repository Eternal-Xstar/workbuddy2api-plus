# start-wb2api.ps1 - thin wrapper（自动启动快捷方式仍指向这里）；实现见 wb2api.ps1
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
& (Join-Path $here 'wb2api.ps1') start