# wb2api.ps1 - workbuddy2api 网关的统一控制台（单一入口）
#
#   .\wb2api.ps1 status              查看服务/账号/指标（默认动作）
#   .\wb2api.ps1 smoke               渠道体检：只测一个 deepseek 模型（一个请求）
#   .\wb2api.ps1 start | stop | restart
#   .\wb2api.ps1 logs [-Tail 50]     看日志尾巴
#   .\wb2api.ps1 backup              备份凭证与配置（保留最近 7 份）
#   .\wb2api.ps1 doctor              一键体检（9 项：配置/进程/端口/健康/账号/目录/磁盘/连通/错误）
#   .\wb2api.ps1 autostart | noautostart
#   .\wb2api.ps1 dsh [status|apply|remove]   配置/撤销 DSH 的 WorkBuddy provider
#   .\wb2api.ps1 dsh models[|-dry|-status]  把网关模型目录同步进该 provider
#   .\wb2api.ps1 wbapp [status|dry|all]    把网关写进 WorkBuddy 应用的自定义模型
#
# 网关是单文件二进制（bin\wb2api.exe，前端已内嵌）；本脚本只做进程与文件操作。
param(
    [Parameter(Position = 0)]
    [ValidateSet('status', 'start', 'stop', 'restart', 'logs', 'backup', 'autostart', 'noautostart', 'dsh', 'doctor', 'wbapp', 'smoke')]
    [string]$Action = 'status',
    [int]$Tail = 50,
    [Parameter(Position = 1)]
    [string]$DshAction = 'status'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root
$exe = Join-Path $root 'bin\wb2api.exe'
$cfgPath = Join-Path $root 'config.json'
$logDir = Join-Path $root 'logs'
$logErr = Join-Path $logDir 'wb2api.log'
$logOut = Join-Path $logDir 'wb2api.out.log'
$backupDir = Join-Path $root 'backups'
$startupLnk = Join-Path ([Environment]::GetFolderPath('Startup')) 'wb2api.lnk'

function Get-ApiKey {
    if (-not (Test-Path $cfgPath)) { return '' }
    try { return (Get-Content $cfgPath -Raw -Encoding UTF8 | ConvertFrom-Json).api_key } catch { return '' }
}

function Get-Listen {
    if (-not (Test-Path $cfgPath)) { return '127.0.0.1:7863' }
    try {
        $l = (Get-Content $cfgPath -Raw -Encoding UTF8 | ConvertFrom-Json).listen
        if (-not $l) { return '127.0.0.1:7863' }
        if ($l.StartsWith(':')) { return "127.0.0.1$l" }
        return $l
    } catch { return '127.0.0.1:7863' }
}

function Invoke-Api([string]$Path) {
    $base = 'http://' + (Get-Listen)
    $key = Get-ApiKey
    $h = @{}
    if ($key) { $h['Authorization'] = "Bearer $key" }
    return Invoke-RestMethod -Uri ($base + $Path) -Headers $h -TimeoutSec 8
}

function Wait-Health([int]$Seconds = 20) {
    $base = 'http://' + (Get-Listen)
    for ($i = 0; $i -lt $Seconds * 2; $i++) {
        Start-Sleep -Milliseconds 500
        try {
            $null = Invoke-WebRequest -Uri ($base + '/healthz') -UseBasicParsing -TimeoutSec 2
            return $true
        } catch {
            # 503 = 服务在跑但没有可用账号（healthz 的预期形态），同样算起来了
            if ($_.Exception.Response) { return $true }
        }
    }
    return $false
}

function Do-Start {
    $p = @(Get-Process -Name wb2api -ErrorAction SilentlyContinue)
    if ($p.Count -gt 0) { "already running: PID $($p.Id -join ',')"; return }
    if (-not (Test-Path $exe)) { throw "missing $exe - build first: go build -o bin\wb2api.exe .\cmd\server" }
    New-Item -ItemType Directory -Force -Path $logDir | Out-Null
    # 主日志（logs\wb2api.log）现在由**进程自己**按大小轮转（config.logging.file），
    # 这里不再动它。启动脚本只兜 stdio：Go 的 panic / 运行时错误 / logging.file 打开
    # 失败前的早期输出都会落到 stdio.log，正常运行时它很小，重启时滚成 .prev。
    $stdioOut = Join-Path $logDir 'wb2api.stdio.log'
    $stdioErr = Join-Path $logDir 'wb2api.stdio.err.log'
    foreach ($f in @($stdioOut, $stdioErr)) {
        if (Test-Path -LiteralPath $f) { Move-Item -LiteralPath $f -Destination ($f + '.prev') -Force }
    }
    $stdinFile = Join-Path $env:TEMP 'wb2api-stdin.txt'
    if (-not (Test-Path $stdinFile)) { New-Item -ItemType File -Path $stdinFile -Force | Out-Null }
    $proc = Start-Process -FilePath $exe -ArgumentList '-config', 'config.json' -WorkingDirectory $root `
        -WindowStyle Hidden -PassThru -RedirectStandardOutput $stdioOut -RedirectStandardError $stdioErr -RedirectStandardInput $stdinFile
    if (Wait-Health 20) {
        $keyState = 'off'
        if (Get-ApiKey) { $keyState = 'on' }
        "started: PID $($proc.Id) -> http://$(Get-Listen)/panel/  (api_key=$keyState)"
    } else {
        "start command sent (PID $($proc.Id)) but /healthz did not answer - check $logErr"
    }
}

function Do-Stop {
    $p = @(Get-Process -Name wb2api -ErrorAction SilentlyContinue)
    if ($p.Count -eq 0) { 'not running'; return }
    $p | Stop-Process -Force
    for ($i = 0; $i -lt 20; $i++) {
        Start-Sleep -Milliseconds 200
        if (@(Get-Process -Name wb2api -ErrorAction SilentlyContinue).Count -eq 0) { break }
    }
    if (@(Get-Process -Name wb2api -ErrorAction SilentlyContinue).Count -gt 0) { 'stop failed: wb2api still present' }
    else { "stopped: PID $($p.Id -join ',')" }
}

function Do-Status {
    $p = @(Get-Process -Name wb2api -ErrorAction SilentlyContinue)
    if ($p.Count -eq 0) { 'service: STOPPED'; return }
    $proc = $p[0]
    $rss = [math]::Round($proc.WorkingSet64 / 1MB, 1)
    $up = (Get-Date) - $proc.StartTime
    $keyState = 'off'
    if (Get-ApiKey) { $keyState = 'on' }
    "service : running  PID $($proc.Id)  RSS ${rss}MB  up $([int]$up.TotalHours)h$($up.Minutes)m"
    "listen  : http://$(Get-Listen)/panel/   api_key=$keyState"
    try {
        $st = Invoke-Api '/status'
        "accounts: $($st.healthy)/$($st.total) healthy · cooling $($st.cooling) · disabled $($st.disabled)"
        if ($st.quota_scan) {
            $pairs = @($st.quota_scan.pairs)
            $blocked = @($pairs | Where-Object { $_.status -eq 429 }).Count
            "quota   : enabled=$($st.quota_scan.enabled) models=$($st.quota_scan.models -join ',') pairs=$($pairs.Count) blocked=$blocked"
        }
        $watch = @($st.upstream_watch | Where-Object { $_.suspected })
        if ($watch.Count -gt 0) {
            "WARN    : upstream drift suspected -> " + (($watch | ForEach-Object { $_.kind }) -join ', ')
        }
    } catch { "status  : /status unavailable ($($_.Exception.Message))" }
    try {
        $m = Invoke-Api '/panel/api/request_metrics'
        $p95 = '-'; if ($m.p95_duration_ms) { $p95 = "$($m.p95_duration_ms)ms" }
        "requests: $($m.completed) done · p95 $p95 · fail5m $($m.window_5m.fail_rate)%"
    } catch { }
}

function Do-Doctor {
    # 一键体检：把"怎么又不行了"拆成 9 项可判定的检查，逐项 PASS/WARN/FAIL。
    # 只读，不改任何东西（唯一副作用是往 auths/ 写一个临时文件再删掉，验证可写）。
    $fail = 0; $warn = 0
    function Line($no, $name, $state, $detail) {
        $tag = switch ($state) { 'PASS' { ' OK ' } 'WARN' { 'WARN' } 'FAIL' { 'FAIL' } default { ' ?? ' } }
        $script:fail += if ($state -eq 'FAIL') { 1 } else { 0 }
        $script:warn += if ($state -eq 'WARN') { 1 } else { 0 }
        "  [{0}] {1,-10} {2}" -f $tag, $name, $detail
    }

    "WorkBuddy 网关体检  $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')"
    "------------------------------------------------------------------"

    # 1 配置
    $cfg = $null
    if (-not (Test-Path $cfgPath)) {
        Line 1 '配置' 'FAIL' "找不到 $cfgPath"
    } else {
        try {
            $cfg = Get-Content $cfgPath -Raw -Encoding UTF8 | ConvertFrom-Json
            $keyState = 'off'; if ($cfg.api_key) { $keyState = 'on' }
            Line 1 '配置' 'PASS' ("config.json 可解析 · listen={0} · api_key={1} · 上传上限生效" -f $cfg.listen, $keyState)
        } catch {
            Line 1 '配置' 'FAIL' ("config.json 解析失败: " + $_.Exception.Message)
        }
    }
    if ($null -eq $cfg) { "------------------------------------------------------------------"; "结论：配置不可用，其余检查略过"; exit 1 }

    # 2 进程
    $procs = @(Get-Process -Name wb2api -ErrorAction SilentlyContinue)
    if ($procs.Count -eq 0) {
        Line 2 '进程' 'FAIL' 'wb2api 未运行 —— 双击桌面「WorkBuddy 网关」或执行 .\wb2api.ps1 start'
    } else {
        $rss = [math]::Round(($procs | Measure-Object WorkingSet64 -Sum).Sum / 1MB, 1)
        Line 2 '进程' 'PASS' ("运行中 PID {0} · RSS {1}MB" -f (($procs.Id) -join ','), $rss)
    }

    # 3 端口
    $port = 7863
    if ($cfg.listen -match ':(\d+)$') { $port = [int]$Matches[1] }
    $own = @(Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)
    if ($own.Count -eq 0) {
        Line 3 '端口' 'FAIL' ("{0} 没有监听 —— 服务没起来或被别的程序占用" -f $port)
    } elseif ($procs.Count -gt 0 -and ($own.OwningProcess -contains $procs[0].Id)) {
        Line 3 '端口' 'PASS' ("127.0.0.1:{0} 由 wb2api 监听" -f $port)
    } else {
        Line 3 '端口' 'WARN' ("{0} 在监听，但占用者不是当前 wb2api 进程（PID {1}）" -f $port, ($own.OwningProcess -join ','))
    }

    # 4 健康端点
    $base = 'http://' + (Get-Listen)
    $hz = '?'
    try { $r = Invoke-WebRequest -Uri ($base + '/healthz') -UseBasicParsing -TimeoutSec 5; $hz = [string]$r.StatusCode }
    catch { if ($_.Exception.Response) { $hz = [string][int]$_.Exception.Response.StatusCode } }
    switch ($hz) {
        '200' { Line 4 '健康检查' 'PASS' '/healthz 200' }
        '503' { Line 4 '健康检查' 'WARN' '/healthz 503（服务在跑，但没有可用账号）' }
        default { Line 4 '健康检查' 'FAIL' ("/healthz 无响应（{0}）" -f $hz) }
    }

    # 5 账号与令牌到期
    $acctFiles = @(Get-ChildItem (Join-Path $root 'auths') -Filter '*.json' -ErrorAction SilentlyContinue)
    $total = $acctFiles.Count
    $healthy = 0; $cooling = 0; $disabled = 0; $soonest = $null; $expired = 0
    try {
        $st = Invoke-Api '/status'
        $healthy = [int]$st.healthy; $cooling = [int]$st.cooling; $disabled = [int]$st.disabled
        if ($st.total) { $total = [int]$st.total }
    } catch { }
    foreach ($f in $acctFiles) {
        try {
            $j = Get-Content $f.FullName -Raw -Encoding UTF8 | ConvertFrom-Json
            $exp = [int64]$j.auth.expiresAt
            if ($exp -le 0) { continue }
            $dt = [DateTimeOffset]::FromUnixTimeSeconds($exp).ToLocalTime()
            if ($dt -le [DateTimeOffset]::Now) { $expired++ }
            if ($null -eq $soonest -or $dt -lt $soonest) { $soonest = $dt }
        } catch { }
    }
    $expNote = '到期未知'
    if ($soonest) { $expNote = '最近到期 ' + $soonest.ToString('yyyy-MM-dd') + "（$([int]($soonest - [DateTimeOffset]::Now).TotalDays) 天）" }
    if ($total -eq 0) {
        Line 5 '账号' 'FAIL' '一个账号都没有 —— 用「添加账号」或导入 JSON'
    } elseif ($healthy -eq 0) {
        Line 5 '账号' 'FAIL' ("{0} 个号全部不可用（冷却 {1} / 禁用 {2}）" -f $total, $cooling, $disabled)
    } elseif ($expired -gt 0) {
        Line 5 '账号' 'WARN' ("{0}/{1} 可用 · {2} 个令牌已过期（会自动刷新，若失败需重新登录）· {3}" -f $healthy, $total, $expired, $expNote)
    } else {
        Line 5 '账号' 'PASS' ("{0}/{1} 可用 · 冷却 {2} · 禁用 {3} · {4}" -f $healthy, $total, $cooling, $disabled, $expNote)
    }

    # 6 凭证目录可写
    $authDir = Join-Path $root 'auths'
    if (-not (Test-Path $authDir)) {
        Line 6 '凭证目录' 'WARN' 'auths/ 不存在（导入或登录时会自动创建）'
    } else {
        $probe = Join-Path $authDir ('.doctor-' + [guid]::NewGuid().ToString('N') + '.tmp')
        try {
            Set-Content -LiteralPath $probe -Value 'ok' -Encoding ASCII -ErrorAction Stop
            Remove-Item -LiteralPath $probe -Force
            Line 6 '凭证目录' 'PASS' ("auths/ 可写 · {0} 个凭证文件" -f $total)
        } catch {
            Line 6 '凭证目录' 'FAIL' ("auths/ 不可写: " + $_.Exception.Message)
        }
    }

    # 7 磁盘与日志体积
    $logBytes = 0
    $logFiles = @()
    foreach ($sub in @('wb2api.log', 'wb2api.log.1', 'wb2api.log.2', 'wb2api.log.3',
                       'wb2api.log.prev', 'wb2api.stdio.log', 'wb2api.stdio.err.log',
                       'wb2api.out.log')) {
        $f = Join-Path $logDir $sub
        if (Test-Path -LiteralPath $f) {
            $sz = (Get-Item -LiteralPath $f).Length
            $logBytes += $sz
            if ($sz -gt 0) {
                $human = if ($sz -ge 1MB) { "{0}MB" -f [math]::Round($sz / 1MB, 1) } else { "{0}KB" -f [math]::Round($sz / 1KB, 1) }
                $logFiles += ("{0} {1}" -f $sub, $human)
            }
        }
    }
    $archBytes = 0
    $archDir = Join-Path $root 'data'
    if (Test-Path $archDir) {
        $archBytes = (Get-ChildItem $archDir -Recurse -File -ErrorAction SilentlyContinue |
            Measure-Object Length -Sum).Sum
    }
    if ($null -eq $archBytes) { $archBytes = 0 }
    $free = $null
    try {
        $drive = (Get-Item $root).PSDrive.Name
        $free = [math]::Round((Get-PSDrive $drive).Free / 1GB, 1)
    } catch { }
    $sizeNote = ("日志 {0}MB（{1}）· data/ {2}MB" -f [math]::Round($logBytes / 1MB, 1), ($logFiles -join ', '), [math]::Round($archBytes / 1MB, 1))
    if ($null -ne $free) { $sizeNote += " · 磁盘剩余 ${free}GB" }
    $state = 'PASS'
    if ($null -ne $free -and $free -lt 2) { $state = 'FAIL' }
    elseif ($logBytes -gt 200MB) { $state = 'WARN' }
    Line 7 '磁盘日志' $state $sizeNote

    # 8 上游连通性（TCP 到 chat 域名 443）
    $targets = @('copilot.tencent.com', 'www.codebuddy.cn')
    $okHosts = @(); $badHosts = @()
    foreach ($h in $targets) {
        $sw = [Diagnostics.Stopwatch]::StartNew()
        $client = New-Object System.Net.Sockets.TcpClient
        try {
            $iar = $client.BeginConnect($h, 443, $null, $null)
            if ($iar.AsyncWaitHandle.WaitOne(3000) -and $client.Connected) {
                $client.EndConnect($iar); $sw.Stop()
                $okHosts += ("{0} {1}ms" -f $h, [int]$sw.ElapsedMilliseconds)
            } else { $badHosts += $h }
        } catch { $badHosts += $h } finally { $client.Close() }
    }
    if ($badHosts.Count -eq 0) {
        Line 8 '上游连通' 'PASS' ($okHosts -join ' · ')
    } elseif ($okHosts.Count -gt 0) {
        Line 8 '上游连通' 'WARN' ("可达: " + ($okHosts -join ', ') + " · 不可达: " + ($badHosts -join ', '))
    } else {
        Line 8 '上游连通' 'FAIL' '两个上游域名都连不上 —— 检查网络/代理/DNS'
    }

    # 9 近 24h 上游错误
    try {
        $es = Invoke-Api '/panel/api/error_summary?hours=24'
        $sev = @($es.buckets | Where-Object { $_.severity -eq 'bad' })
        $wrn = @($es.buckets | Where-Object { $_.severity -eq 'warn' })
        if ($sev.Count -gt 0) {
            Line 9 '近24h错误' 'FAIL' (($sev | ForEach-Object { ($_.code, ($_.kind), ('×' + $_.count)) -join ' ' }) -join ' · ')
        } elseif ($wrn.Count -gt 0) {
            Line 9 '近24h错误' 'WARN' (($wrn | ForEach-Object { ($_.code, ($_.kind), ('×' + $_.count)) -join ' ' }) -join ' · ')
        } else {
            Line 9 '近24h错误' 'PASS' ("无告警级错误（共 " + $es.total + " 次，均为提示级）")
        }
    } catch {
        Line 9 '近24h错误' 'WARN' '接口不可用（服务未运行？）'
    }

    "------------------------------------------------------------------"
    if ($fail -gt 0) { "结论：$fail 项失败、$warn 项警告 —— 先修 FAIL 项。" }
    elseif ($warn -gt 0) { "结论：全部通过，但有 $warn 项警告（可以继续用，留意即可）。" }
    else { "结论：全部通过，可以正常使用。" }
    if ($fail -gt 0) { exit 1 }
}
function Do-Logs {
    if (-not (Test-Path $logErr)) { 'no log file yet'; return }
    Get-Content $logErr -Encoding UTF8 -Tail $Tail
}

function Do-Backup {
    New-Item -ItemType Directory -Force -Path $backupDir | Out-Null
    $ts = Get-Date -Format 'yyyyMMdd-HHmmss'
    $zip = Join-Path $backupDir "wb2api-$ts.zip"
    $stage = Join-Path $env:TEMP "wb2api-backup-$ts"
    New-Item -ItemType Directory -Force -Path (Join-Path $stage 'auths') | Out-Null
    New-Item -ItemType Directory -Force -Path (Join-Path $stage 'data') | Out-Null
    # 只备"丢了就要重新扫码"的东西：凭证、配置、池状态与账本；日志与请求归档不进包
    if (Test-Path (Join-Path $root 'auths')) {
        Copy-Item (Join-Path $root 'auths\*.json') (Join-Path $stage 'auths') -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path $cfgPath) { Copy-Item $cfgPath $stage -Force }
    foreach ($f in @('state.json', 'usage.json', 'credit-history.json', 'quota-scan.json')) {
        $src = Join-Path $root "data\$f"
        if (Test-Path $src) { Copy-Item $src (Join-Path $stage 'data') -Force }
    }
    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip -Force
    Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
    $size = [math]::Round((Get-Item $zip).Length / 1KB, 1)
    "backup: $zip (${size}KB)"
    $old = Get-ChildItem $backupDir -Filter 'wb2api-*.zip' | Sort-Object LastWriteTime -Descending | Select-Object -Skip 7
    if ($old) { $old | Remove-Item -Force; "pruned: $($old.Count) old backup(s)" }
}

function Do-Autostart([bool]$on) {
    if ($on) {
        $ws = New-Object -ComObject WScript.Shell
        $lnk = $ws.CreateShortcut($startupLnk)
        $lnk.TargetPath = "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe"
        $lnk.Arguments = "-WindowStyle Hidden -ExecutionPolicy Bypass -File `"$root\start-wb2api.ps1`""
        $lnk.WorkingDirectory = $root
        $lnk.Description = 'workbuddy2api gateway autostart'
        $lnk.Save()
        "autostart: ON -> $startupLnk"
    } else {
        if (Test-Path $startupLnk) { Remove-Item $startupLnk -Force; "autostart: OFF (removed $startupLnk)" }
        else { 'autostart: already off' }
    }
}

function Do-Dsh([string]$sub) {
    # models / models-dry / models-status 走 dsh_sync_models.py（把网关模型目录同步进
    # DSH 的 workbuddy-local provider）；其余走 dsh_connect.py（增删 provider 本身）。
    if ($sub -like 'models*') {
        $py = Join-Path $root 'scripts\dsh_sync_models.py'
        if (-not (Test-Path $py)) { throw "missing $py" }
        $arg = ''
        if ($sub -eq 'models-dry') { $arg = '--dry-run' }
        if ($sub -eq 'models-status') { $arg = '--status' }
        & python $py $arg
        if ($LASTEXITCODE -ne 0) { throw "dsh_sync_models.py $arg failed" }
        if ($sub -eq 'models') { 'DSH: 重启 DeepSeek Harness 后模型下拉才会更新' }
        return
    }
    $py = Join-Path $root 'scripts\dsh_connect.py'
    if (-not (Test-Path $py)) { throw "missing $py" }
    & python $py $sub
    if ($LASTEXITCODE -ne 0) { throw "dsh_connect.py $sub failed" }
    if ($sub -eq 'apply') { 'DSH: 重启 DeepSeek Harness 后，模型选择器会出现 WorkBuddy（本机 7863）' }
}

function Do-WbApp([string]$sub) {
    # 把本机网关写进 WorkBuddy 桌面应用的「自定义模型」（~/.workbuddy/models.json）。
    $py = Join-Path $root 'scripts\wbapp_sync_models.py'
    if (-not (Test-Path $py)) { throw "missing $py" }
    $arg = ''
    if ($sub -eq 'dry') { $arg = '--dry-run' }
    if ($sub -eq 'status') { $arg = '--list' }
    if ($sub -eq 'all') { $arg = '--all' }
    & python $py $arg
    if ($LASTEXITCODE -ne 0) { throw "wbapp_sync_models.py $arg failed" }
    if ($sub -eq '' -or $sub -eq 'all') { 'WorkBuddy 应用：完全退出并重启后，模型下拉里会出现「(WorkBuddy 本机)」条目' }
}
function Do-Smoke {
    # 渠道体检：**只发一个请求**，deepseek-v4.1-flash，够判断号池通不通。
    # 为什么只测一个（见 AGENTS.md）：上游免费/低倍率模型有排队（6020/6021，排位可到
    # 1700+），批量试跑会把自己拖进长队、把多个号同时冷却，还会把"通不通"这个问题搅浑。
    $base = 'http://' + (Get-Listen)
    $key = Get-ApiKey
    $h = @{ 'Content-Type' = 'application/json' }
    if ($key) { $h['Authorization'] = "Bearer $key" }
    $body = '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"只回答两个字：收到"}],"max_tokens":64,"stream":false}'
    $t0 = Get-Date
    try {
        $r = Invoke-RestMethod -Uri ($base + '/v1/chat/completions') -Method Post -Headers $h `
            -Body ([Text.Encoding]::UTF8.GetBytes($body)) -TimeoutSec 120
        $ms = [int]((Get-Date) - $t0).TotalMilliseconds
        $txt = [string]$r.choices[0].message.content
        # PS 5.1 的 Invoke-RestMethod 在响应头没带 charset 时按 Latin-1 解码 UTF-8 → 乱码。
        # 把已解码的字符按 Latin-1 还原成字节，再用 UTF-8 解一次。
        if ($txt -match '[^\x00-\x7F]') {
            try {
                $bytes = [Text.Encoding]::GetEncoding(28591).GetBytes($txt)
                $fixed = [Text.Encoding]::UTF8.GetString($bytes)
                if ($fixed -notmatch '\uFFFD') { $txt = $fixed }
            } catch { }
        }
        $txt = ($txt -replace "`r?`n", ' ')
        if (-not $txt) { $txt = '(空回复)' }
        $tok = 0
        if ($r.usage) { $tok = $r.usage.completion_tokens }
        "smoke : OK  model=deepseek-v4.1-flash  ${ms}ms  out=${tok}tok  content=`"$txt`""
    } catch {
        $ms = [int]((Get-Date) - $t0).TotalMilliseconds
        $msg = $_.Exception.Message
        $detail = ''
        try {
            $sr = New-Object IO.StreamReader($_.Exception.Response.GetResponseStream())
            $detail = $sr.ReadToEnd(); $sr.Close()
            $j = $detail | ConvertFrom-Json
            if ($j.error) { $detail = ($j.error.code + ' · ' + $j.error.message) }
        } catch { }
        if (-not $detail) { $detail = $msg }
        "smoke : FAIL  ${ms}ms  $detail"
        "        先看 .\wb2api.ps1 status（是否全池冷却）与 .\wb2api.ps1 logs -Tail 20"
        return
    }
}
switch ($Action) {
    'start'       { Do-Start }
    'stop'        { Do-Stop }
    'restart'     { Do-Stop; Start-Sleep -Seconds 1; Do-Start }
    'status'      { Do-Status }
    'logs'        { Do-Logs }
    'backup'      { Do-Backup }
    'autostart'   { Do-Autostart $true }
    'noautostart' { Do-Autostart $false }
    'dsh'         { Do-Dsh $DshAction }
    'doctor'      { Do-Doctor }
    'wbapp'       { Do-WbApp $DshAction }
    'smoke'       { Do-Smoke }
}
