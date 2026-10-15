<#
.SYNOPSIS
    统一执行 Go、网页与构建脚本测试，不启动生产服务。
.DESCRIPTION
    默认运行三类测试；任一步失败即停止，不把缺失工具视为通过。
    恢复调用者的工作目录与 Go 环境；审计写入项目 logs/。
.PARAMETER Suite
    all（默认）、go、web 或 build。
.EXAMPLE
    .\scripts\test.ps1
    .\scripts\test.ps1 -Suite go
#>
param(
    [ValidateSet('all', 'go', 'web', 'build')]
    [string]$Suite = 'all'
)

$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$trace = [Guid]::NewGuid().ToString('N')
$encoding = [Text.UTF8Encoding]::new($false)
$logDirectory = Join-Path $root 'logs'
[IO.Directory]::CreateDirectory($logDirectory) | Out-Null
$auditPath = Join-Path $logDirectory ("test-runner-{0}.jsonl" -f (Get-Date -Format 'yyyy-MM-dd'))
$environmentNames = @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOARM', 'GOAMD64', 'GO386', 'GOARM64',
    'WORKBUDDY_BROWSER_FIXTURE', 'WORKBUDDY_HEALTH_BROWSER_FIXTURE', 'WORKBUDDY_BROWSER_DURATION')
$previousEnvironment = @{}
foreach ($name in $environmentNames) {
    $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

function Protect-TestAudit {
    param([string]$Text)
    $Text = $Text -replace 'github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9]+', '[REDACTED]'
    $Text = $Text -replace '(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+', '$1[REDACTED]'
    return $Text -replace '(https?://)[^/\s:@]+:[^@\s/]+@', '$1[REDACTED]@'
}

function Write-TestAudit {
    param([string]$Event, [string]$Description, [hashtable]$Fields = @{})
    $record = [ordered]@{
        time = [DateTimeOffset]::Now.ToString('o')
        trace_id = $trace
        event = $Event
        description = $Description
    }
    foreach ($key in $Fields.Keys) { $record[$key] = $Fields[$key] }
    $line = Protect-TestAudit ($record | ConvertTo-Json -Compress -Depth 8)
    [IO.File]::AppendAllText($auditPath, $line + "`n", $encoding)
}

function Invoke-TestTool {
    param([string]$Tool, [string[]]$Arguments, [string]$Phase)
    $command = Get-Command $Tool -ErrorAction Stop
    $PSNativeCommandUseErrorActionPreference = $false
    Write-TestAudit 'tool_started' '开始测试阶段，仅记录命令参数，不输出环境变量' @{
        phase = $Phase; tool = $Tool; arguments = $Arguments
    }
    $output = @(& $command @Arguments 2>&1)
    $code = $LASTEXITCODE
    foreach ($line in $output) { Write-Host (Protect-TestAudit ([string]$line)) }
    Write-TestAudit 'tool_completed' '测试阶段结束，输出已脱敏并落盘' @{
        phase = $Phase; exit_code = $code; output = ($output -join "`n")
    }
    if ($code -ne 0) { throw "$Phase 测试失败，退出码 $code；详见 $auditPath" }
}

Push-Location -LiteralPath $root
try {
    Write-TestAudit 'tests_requested' '运行本地测试，不部署、不推送、不触发真实价格探测' @{ suite = $Suite }
    if ($Suite -in @('all', 'go')) {
        foreach ($name in $environmentNames) {
            [Environment]::SetEnvironmentVariable($name, $null, 'Process')
        }
        [Environment]::SetEnvironmentVariable('CGO_ENABLED', '0', 'Process')
        Invoke-TestTool 'go' @('vet', './...') 'go_vet'
        Invoke-TestTool 'go' @('test', './...', '-count=1') 'go_tests'
    }
    if ($Suite -in @('all', 'web')) {
        Invoke-TestTool 'node' @('--check', 'web/app.js') 'web_syntax'
        Invoke-TestTool 'node' @('--test', 'tests/web/app.test.cjs') 'web_tests'
    }
    if ($Suite -in @('all', 'build')) {
        Write-TestAudit 'build_tests_started' '调用模拟构建回归；不会发起真实交叉编译' @{}
        & (Join-Path $root 'tests/build/build_script.test.ps1')
        Write-TestAudit 'build_tests_completed' '构建矩阵、清理边界和环境恢复测试通过' @{}
    }
    Write-TestAudit 'tests_completed' '所选测试全部通过' @{ suite = $Suite }
    # 模拟构建测试包含预期的非零退出码；不能把该状态泄漏成整套测试失败。
    $global:LASTEXITCODE = 0
    Write-Host "测试全部通过；审计：$auditPath"
} catch {
    $global:LASTEXITCODE = 1
    Write-TestAudit 'tests_failed' '测试失败或工具缺失，没有跳过失败阶段' @{
        suite = $Suite; error = $_.Exception.Message
    }
    throw
} finally {
    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process')
    }
    Pop-Location
}
