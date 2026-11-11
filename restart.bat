@echo off
setlocal
set "CANVAS_BAT_SELF=%~f0"
set "CANVAS_BAT_ROOT=%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$repoRoot = $env:CANVAS_BAT_ROOT.TrimEnd('\'); Invoke-Expression ((Get-Content -LiteralPath $env:CANVAS_BAT_SELF -Encoding UTF8 | Select-Object -Skip 7) -join [char]10); exit $LASTEXITCODE"
if not "%CANVAS_RESTART_DRY_RUN%"=="1" if %ERRORLEVEL% neq 0 pause
exit /b %ERRORLEVEL%
$ErrorActionPreference = "Stop"

try {
    $userBun = Join-Path $env:USERPROFILE ".bun\bin"
    $env:Path = (@($env:Path, [Environment]::GetEnvironmentVariable("Path", "Machine"), [Environment]::GetEnvironmentVariable("Path", "User"), $userBun) | Where-Object { $_ }) -join ";"

    $ports = @(3000, 8080)

    function Get-PortListeningPids([int]$Port) {
        $pids = @()
        try {
            $conns = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction Stop
            if ($conns) {
                $pids += ($conns | Select-Object -ExpandProperty OwningProcess)
            }
        } catch {
            foreach ($line in (cmd.exe /d /c "netstat -ano -p tcp 2>nul")) {
                if ($line -match "^\s*TCP\s+\S+:$Port\s+\S+\s+LISTENING\s+(\d+)\s*$") {
                    $pids += [int]$Matches[1]
                }
            }
        }
        return @($pids | Where-Object { $_ -gt 4 } | Select-Object -Unique)
    }

    function Test-OwnPortHttp([int]$Port) {
        try {
            if ($Port -eq 8080) {
                $r = Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/health/live" -TimeoutSec 2
                return ($r.code -eq 0 -and $null -ne $r.data.build)
            }
            if ($Port -eq 3000) {
                $r = Invoke-WebRequest -Uri "http://127.0.0.1:3000/" -UseBasicParsing -TimeoutSec 2
                return ($r.Content -like "*yingce:theme_store*")
            }
        } catch {}
        return $false
    }

    $allProcs = @{}
    try {
        foreach ($p in (Get-CimInstance Win32_Process -ErrorAction Stop)) {
            $allProcs[[int]$p.ProcessId] = $p
        }
    } catch {}

    $selfPids = @{}
    $cur = [int]$PID
    while ($cur -gt 0 -and -not $selfPids.ContainsKey($cur)) {
        $selfPids[$cur] = $true
        $cur = if ($allProcs.ContainsKey($cur)) { [int]$allProcs[$cur].ParentProcessId } else { 0 }
    }

    $killPids = [System.Collections.Generic.HashSet[int]]::new()

    foreach ($port in $ports) {
        $ownerPids = Get-PortListeningPids $port
        if ($ownerPids.Count -eq 0) {
            Write-Host "[restart] 端口 $port 空闲。" -ForegroundColor DarkGray
            continue
        }

        foreach ($ownerPid in $ownerPids) {
            $matchedRootPid = 0
            $walk = [int]$ownerPid
            $visited = @{}
            while ($walk -gt 4 -and -not $visited.ContainsKey($walk) -and -not $selfPids.ContainsKey($walk) -and $allProcs.ContainsKey($walk)) {
                $visited[$walk] = $true
                $proc = $allProcs[$walk]
                $text = "$($proc.ExecutablePath) $($proc.CommandLine)"
                if ($text.IndexOf($repoRoot, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) {
                    $matchedRootPid = [int]$proc.ProcessId
                }
                $walk = [int]$proc.ParentProcessId
            }

            $gp = try { Get-Process -Id $ownerPid -ErrorAction Stop } catch { $null }
            if ($matchedRootPid -eq 0 -and $gp -and $gp.Path -and $gp.Path.IndexOf($repoRoot, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) {
                $matchedRootPid = [int]$ownerPid
            }

            if ($matchedRootPid -eq 0 -and (Test-OwnPortHttp $port)) {
                $matchedRootPid = [int]$ownerPid
            }

            if ($matchedRootPid -eq 0) {
                $procName = if ($gp) { $gp.ProcessName } elseif ($allProcs.ContainsKey([int]$ownerPid)) { $allProcs[[int]$ownerPid].Name } else { "unknown" }
                $procPath = if ($gp -and $gp.Path) { $gp.Path } elseif ($allProcs.ContainsKey([int]$ownerPid)) { $allProcs[[int]$ownerPid].CommandLine } else { "" }
                throw "端口 $port 已被其他程序占用，拒绝强杀：$procName (PID=$ownerPid) $procPath"
            }

            [void]$killPids.Add($matchedRootPid)
            [void]$killPids.Add([int]$ownerPid)
            Write-Host "[restart] 检测到端口 $port 运行着本项目实例 (PID=$ownerPid)，准备停止..." -ForegroundColor Yellow
        }
    }

    if ($killPids.Count -gt 0) {
        foreach ($kpid in $killPids) {
            if (Get-Process -Id $kpid -ErrorAction SilentlyContinue) { cmd.exe /d /c "taskkill /F /T /PID $kpid >nul 2>nul" }
        }
        $deadline = (Get-Date).AddSeconds(5)
        while ((Get-Date) -lt $deadline) {
            $stillListening = $ports | Where-Object { (Get-PortListeningPids $_).Count -gt 0 }
            if (-not $stillListening) { break }
            Start-Sleep -Milliseconds 200
        }
    }

    if ($env:CANVAS_RESTART_DRY_RUN -eq "1") { exit 0 }

    $scriptsDir = Join-Path $repoRoot "scripts"
    $startCode = (Get-Content -LiteralPath (Join-Path $scriptsDir "start-local.ps1") -Encoding UTF8) -join [char]10
    $startCode = $startCode -replace '\$PSScriptRoot\b', ("'" + $scriptsDir.Replace("'", "''") + "'")
    & ([scriptblock]::Create($startCode))
} catch {
    Write-Host "[ERROR] $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
