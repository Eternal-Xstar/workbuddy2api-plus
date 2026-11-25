# stop-wb2api.ps1 - thin wrapper；实现见 wb2api.ps1
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
& (Join-Path $here 'wb2api.ps1') stop