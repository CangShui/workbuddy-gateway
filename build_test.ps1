<#
.SYNOPSIS
    构建脚本回归测试：使用模拟 Go 命令验证目标矩阵、失败处理、环境恢复与清理边界。
.DESCRIPTION
    此测试不替代真实交叉编译。文件仅写入项目 .codex-tmp/ 与 logs/，不清理用户目录。
#>
$ErrorActionPreference = 'Stop'
$projectRoot = $PSScriptRoot
$testRoot = Join-Path $projectRoot ('.codex-tmp/build-script-tests-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($testRoot) | Out-Null
$scriptPath = Join-Path $projectRoot 'build.ps1'
$testLog = Join-Path $projectRoot ('logs/build-tests-' + (Get-Date -Format 'yyyy-MM-dd') + '.jsonl')
[IO.Directory]::CreateDirectory((Split-Path $testLog)) | Out-Null
$encoding = [Text.UTF8Encoding]::new($false)
$testTrace = [Guid]::NewGuid().ToString('N')
$oldFunction = Get-Item -LiteralPath Function:\go -ErrorAction SilentlyContinue
$envNames = @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOARM', 'GOAMD64', 'GO386', 'GOARM64')
$original = @{}
foreach ($name in $envNames) { $original[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }

function Write-TestResult {
    param([string]$Name)
    $row = @{ time = [DateTimeOffset]::Now.ToString('o'); trace_id = $testTrace; event = 'build_regression_passed'; test = $Name }
    [IO.File]::AppendAllText($testLog, ($row | ConvertTo-Json -Compress) + "`n", $encoding)
    Write-Host "PASS $Name"
}

function Assert-Build {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw $Message }
}

function global:go {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    $global:LASTEXITCODE = 0
    $global:BuildMockCalls += ,@{
        args = $Arguments; os = $env:GOOS; arch = $env:GOARCH; cgo = $env:CGO_ENABLED
        arm = $env:GOARM; amd64 = $env:GOAMD64; arm64 = $env:GOARM64
    }
    if ($global:BuildMockFail -eq $Arguments[0]) {
        $global:LASTEXITCODE = 7
        Write-Output 'intentional simulated Go failure'
        return
    }
    switch ($Arguments[0]) {
        'version' { Write-Output 'go version go1.26.5 windows/amd64' }
        'tool' {
            Write-Output @('windows/386', 'windows/amd64', 'windows/arm64', 'linux/386', 'linux/amd64', 'linux/arm', 'linux/arm64', 'darwin/amd64', 'darwin/arm64')
        }
        'build' {
            $index = [Array]::IndexOf($Arguments, '-o')
            if ($index -lt 0) { throw '模拟构建没有 -o 参数' }
            [IO.File]::WriteAllText($Arguments[$index + 1], "mock $env:GOOS/$env:GOARCH GOARM=$env:GOARM")
        }
    }
}

function Test-ExpectedFailure {
    param([string]$Name, [hashtable]$Arguments)
    $thrown = $false
    try { & $scriptPath @Arguments } catch { $thrown = $true }
    Assert-Build $thrown "$Name 未拒绝错误输入"
    Write-TestResult $Name
}

try {
    $global:BuildMockFail = ''
    $global:BuildMockCalls = @()
    $allPath = Join-Path $testRoot 'all'
    & $scriptPath -OutputDir $allPath
    $manifest = Get-Content -LiteralPath (Join-Path $allPath 'build-manifest.json') -Raw | ConvertFrom-Json
    Assert-Build ($manifest.artifacts.Count -eq 9) '默认目标数不是 9'
    Assert-Build (@($manifest.artifacts | Where-Object { $_.goos -eq 'windows' }).Count -eq 3) 'Windows 目标数错误'
    Assert-Build (@($manifest.artifacts | Where-Object { $_.goos -eq 'linux' }).Count -eq 4) 'Linux 目标数错误'
    Assert-Build (@($manifest.artifacts | Where-Object { $_.goarch -eq 'arm' })[0].goarm -eq '7') 'ARM 默认值不是 7'
    Assert-Build (@(Get-Content -LiteralPath (Join-Path $allPath 'SHA256SUMS')).Count -eq 11) '校验和未覆盖全部二进制/清单/示例'
    Write-TestResult 'default-matrix-and-checksums'

    $values = @{ GOOS = 'freebsd'; GOARCH = 'arm'; CGO_ENABLED = '1'; GOARM = '6'; GOAMD64 = 'v3'; GO386 = 'softfloat'; GOARM64 = 'v8.2' }
    foreach ($name in $envNames) { [Environment]::SetEnvironmentVariable($name, $values[$name], 'Process') }
    $beforeLocation = (Get-Location).Path
    $global:BuildMockCalls = @()
    & $scriptPath -Only windows -OutputDir (Join-Path $testRoot 'windows')
    foreach ($name in $envNames) {
        Assert-Build ([Environment]::GetEnvironmentVariable($name, 'Process') -eq $values[$name]) "成功后环境未恢复：$name"
    }
    Assert-Build ((Get-Location).Path -eq $beforeLocation) '成功后工作目录未恢复'
    $checks = @($global:BuildMockCalls | Where-Object { $_.args[0] -in @('vet', 'test') })
    Assert-Build ($checks.Count -eq 2 -and -not $checks[0].os -and -not $checks[0].arch -and $checks[0].cgo -eq '0') '检查阶段未清空交叉编译环境'
    $builds = @($global:BuildMockCalls | Where-Object { $_.args[0] -eq 'build' })
    Assert-Build ($builds.Count -eq 3) 'Windows 过滤没有输出三个目标'
    Write-TestResult 'windows-filter-and-success-environment-restore'

    & $scriptPath -Only linux -Architecture arm -ArmVersion 6 -SkipTests -OutputDir (Join-Path $testRoot 'arm6')
    Assert-Build (Test-Path -LiteralPath (Join-Path $testRoot 'arm6/workbuddy-gateway-linux-armv6')) 'ARMv6 文件名错误'
    Write-TestResult 'linux-arm-version-filter'

    [IO.File]::WriteAllText((Join-Path $allPath 'keep.txt'), 'keep')
    [IO.Directory]::CreateDirectory((Join-Path $allPath 'nested')) | Out-Null
    [IO.File]::WriteAllText((Join-Path $allPath 'nested/config.json'), 'keep nested')
    & $scriptPath -Only windows -Architecture amd64 -SkipTests -Clean -OutputDir $allPath
    Assert-Build ((Get-Content -LiteralPath (Join-Path $allPath 'keep.txt')) -eq 'keep') 'Clean 删除了无关文件'
    Assert-Build (Test-Path -LiteralPath (Join-Path $allPath 'nested/config.json')) 'Clean 递归清理了子目录'
    Assert-Build (@(Get-ChildItem -LiteralPath $allPath -Filter 'workbuddy-gateway-*' -File).Count -eq 1) '旧构建文件未清理'
    Write-TestResult 'clean-only-known-files'

    Test-ExpectedFailure 'reject-windows-arm32' @{ Only = 'windows'; Architecture = 'arm'; SkipTests = $true; OutputDir = (Join-Path $testRoot 'unsupported') }
    Test-ExpectedFailure 'reject-project-root-clean' @{ SkipTests = $true; Clean = $true; OutputDir = $projectRoot }
    Test-ExpectedFailure 'reject-outside-project' @{ SkipTests = $true; Clean = $true; OutputDir = (Join-Path ([IO.Path]::GetTempPath()) ('build-outside-' + [Guid]::NewGuid())) }

    $global:BuildMockFail = 'build'
    Test-ExpectedFailure 'go-build-failure' @{ Only = 'linux'; Architecture = 'amd64'; SkipTests = $true; OutputDir = (Join-Path $testRoot 'failed') }
    foreach ($name in $envNames) {
        Assert-Build ([Environment]::GetEnvironmentVariable($name, 'Process') -eq $values[$name]) "失败后环境未恢复：$name"
    }
    Assert-Build ((Get-Location).Path -eq $beforeLocation) '失败后工作目录未恢复'
    Assert-Build (-not (Test-Path -LiteralPath (Join-Path $testRoot 'failed/build-manifest.json'))) '失败构建生成了成功清单'
    Assert-Build (@(Get-ChildItem -LiteralPath (Join-Path $testRoot 'failed') -Filter '*.tmp' -File).Count -eq 0) '失败构建遗留了暂存文件'
    Write-TestResult 'failure-environment-and-partial-output'
    Write-Host "全部构建脚本回归通过；日志：$testLog"
} catch {
    $row = @{ time = [DateTimeOffset]::Now.ToString('o'); trace_id = $testTrace; event = 'build_regression_failed'; error = $_.Exception.Message }
    [IO.File]::AppendAllText($testLog, ($row | ConvertTo-Json -Compress) + "`n", $encoding)
    throw
} finally {
    foreach ($name in $envNames) { [Environment]::SetEnvironmentVariable($name, $original[$name], 'Process') }
    Remove-Item -LiteralPath Function:\go -ErrorAction SilentlyContinue
    if ($oldFunction) { Set-Item -LiteralPath Function:\go -Value $oldFunction.ScriptBlock }
}
