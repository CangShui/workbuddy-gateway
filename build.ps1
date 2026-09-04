<#
.SYNOPSIS
    WorkBuddy Local Gateway 一键交叉编译脚本（纯 Go、零 CGO 依赖，产物为静态单二进制）

.DESCRIPTION
    默认构建全部 5 个平台产物到 dist\ 目录：
        workbuddy-gateway-windows-amd64.exe
        workbuddy-gateway-linux-amd64
        workbuddy-gateway-linux-arm64
        workbuddy-gateway-darwin-amd64
        workbuddy-gateway-darwin-arm64

    构建前默认执行 go vet + go test，任何一步失败即中止。

.PARAMETER Only
    仅构建指定平台：windows | linux | darwin

.PARAMETER OutputDir
    输出目录（默认 dist）

.PARAMETER Clean
    构建前清空输出目录

.PARAMETER SkipTests
    跳过构建前的 go vet / go test

.EXAMPLE
    .\build.ps1                 # 全平台构建
    .\build.ps1 -Only windows   # 只构建 windows
    .\build.ps1 -Clean -SkipTests
#>
param(
    [ValidateSet('', 'windows', 'linux', 'darwin')]
    [string]$Only = '',
    [string]$OutputDir = 'dist',
    [switch]$Clean,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

# ---------------------------------------------------------------------------
# 0. 环境检查
# ---------------------------------------------------------------------------
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    Write-Host '[错误] 未检测到 Go 工具链，请先安装 Go 1.20+：https://go.dev/dl/' -ForegroundColor Red
    exit 1
}

# 从 main.go 读取版本号（仅用于展示；版本定义在 main.go 的 version 常量中）
$version = 'dev'
if (Test-Path 'main.go') {
    $m = Select-String -Path 'main.go' -Pattern 'version\s*=\s*"([^"]+)"' | Select-Object -First 1
    if ($m) { $version = $m.Matches[0].Groups[1].Value }
}

Write-Host '================ WorkBuddy Local Gateway 构建 ================' -ForegroundColor Cyan
Write-Host ("工具链: {0}" -f (go version))
Write-Host ("目标版本: v{0}" -f $version)

# ---------------------------------------------------------------------------
# 1. 代码检查与单元测试
# ---------------------------------------------------------------------------
if (-not $SkipTests) {
    Write-Host "`n[1/3] go vet + go test ..." -ForegroundColor Cyan
    go vet .
    if ($LASTEXITCODE -ne 0) { Write-Host '[错误] go vet 未通过' -ForegroundColor Red; exit 1 }
    go test ./...
    if ($LASTEXITCODE -ne 0) { Write-Host '[错误] go test 未通过' -ForegroundColor Red; exit 1 }
}
else {
    Write-Host "`n[1/3] 已跳过 go vet / go test (-SkipTests)" -ForegroundColor DarkGray
}

# ---------------------------------------------------------------------------
# 2. 交叉编译
# ---------------------------------------------------------------------------
$targets = @(
    @{ GOOS = 'windows'; GOARCH = 'amd64'; Ext = '.exe' }
    @{ GOOS = 'linux';   GOARCH = 'amd64'; Ext = '' }
    @{ GOOS = 'linux';   GOARCH = 'arm64'; Ext = '' }
    @{ GOOS = 'darwin';  GOARCH = 'amd64'; Ext = '' }
    @{ GOOS = 'darwin';  GOARCH = 'arm64'; Ext = '' }
)
if ($Only) { $targets = @($targets | Where-Object { $_.GOOS -eq $Only }) }

if (-not (Test-Path -LiteralPath $OutputDir)) {
    New-Item -ItemType Directory -Path $OutputDir | Out-Null
}
elseif ($Clean) {
    Remove-Item (Join-Path $OutputDir '*') -Recurse -Force
    Write-Host ("已清空输出目录: {0}" -f $OutputDir) -ForegroundColor DarkGray
}

# 保存并临时覆盖构建环境变量，结束后恢复
$oldEnv = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
$env:CGO_ENABLED = '0'   # 强制零 CGO，保证产物可任意交叉分发

$failed = @()
$total = $targets.Count
$i = 0
foreach ($t in $targets) {
    $i++
    $name = "workbuddy-gateway-$($t.GOOS)-$($t.GOARCH)$($t.Ext)"
    Write-Host ("`n[2/3] ({0}/{1}) building {2} ..." -f $i, $total, $name) -ForegroundColor Cyan
    $env:GOOS = $t.GOOS
    $env:GOARCH = $t.GOARCH
    go build -trimpath -ldflags '-s -w' -o (Join-Path $OutputDir $name) .
    if ($LASTEXITCODE -ne 0) {
        $failed += $name
        Write-Host "  [FAIL] $name" -ForegroundColor Red
    }
    else {
        Write-Host "  [OK]   $name" -ForegroundColor Green
    }
}

# 恢复原环境变量
foreach ($k in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
    if ($oldEnv[$k]) { Set-Item -Path "env:$k" -Value $oldEnv[$k] }
    else { Remove-Item -Path "env:$k" -ErrorAction SilentlyContinue }
}

# ---------------------------------------------------------------------------
# 3. 结果汇总
# ---------------------------------------------------------------------------
Write-Host "`n================ 构建结果 ================" -ForegroundColor Cyan
if ($failed.Count -gt 0) {
    Write-Host ("失败 {0} 个: {1}" -f $failed.Count, ($failed -join ', ')) -ForegroundColor Red
    exit 1
}

Get-ChildItem -LiteralPath $OutputDir -File |
    Sort-Object Name |
    Format-Table @(
        @{ Label = 'Artifact'; Expression = { '  ' + $_.Name } },
        @{ Label = 'Size';     Expression = { '{0:N1} MB' -f ($_.Length / 1MB) } }
    ) -AutoSize | Out-Host

Write-Host ("全部构建成功  共 {0} 个产物，输出目录: {1}" -f $total, (Join-Path $PSScriptRoot $OutputDir)) -ForegroundColor Green
