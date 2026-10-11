<#
.SYNOPSIS
    WorkBuddy Local Gateway 一键交叉编译（纯 Go、零 CGO）。
.DESCRIPTION
    默认构建 9 个产物：Windows 386/amd64/arm64，Linux 386/amd64/arm/arm64，
    macOS amd64/arm64。Linux ARM32 默认 GOARM=7，文件名含 armv7。
    Windows ARM32 不受 Go 工具链支持，不生成虚假产物。
    默认先执行 go vet + go test；输出 SHA256SUMS、build-manifest.json、config.example.json。
    构建审计写入项目 logs/；只清理已知构建文件，不递归删除目录。
.PARAMETER Only
    仅构建 windows、linux 或 darwin。
.PARAMETER Architecture
    仅构建 386、amd64、arm 或 arm64；组合必须存在受支持目标。
.PARAMETER ArmVersion
    Linux ARM32 最低 ARM 版本：5、6 或 7，默认 7。
.PARAMETER OutputDir
    输出目录，默认 dist；必须是项目目录内的独立子目录，不能是符号链接。
.PARAMETER Clean
    清理已知构建产物、清单和示例配置，保留其他文件及子目录。
.PARAMETER SkipTests
    跳过构建前检查，仅用于已完成检查后的重复构建。
.EXAMPLE
    .\build.ps1
    .\build.ps1 -Only linux -Architecture arm -ArmVersion 6
#>
param(
    [ValidateSet('', 'windows', 'linux', 'darwin')]
    [string]$Only = '',
    [ValidateSet('', '386', 'amd64', 'arm', 'arm64')]
    [string]$Architecture = '',
    [ValidateSet('5', '6', '7')]
    [string]$ArmVersion = '7',
    [string]$OutputDir = 'dist',
    [switch]$Clean,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
$trace = [Guid]::NewGuid().ToString('N')
$logDir = Join-Path $root 'logs'
[IO.Directory]::CreateDirectory($logDir) | Out-Null
$auditFile = Join-Path $logDir ("build-{0}.jsonl" -f (Get-Date -Format 'yyyy-MM-dd'))
$utf8 = [Text.UTF8Encoding]::new($false)
$environmentNames = @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOARM', 'GOAMD64', 'GO386', 'GOARM64')
$oldEnvironment = @{}
foreach ($key in $environmentNames) {
    $oldEnvironment[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
}

function Write-BuildAudit {
    param([string]$Event, [string]$Detail, [hashtable]$Fields = @{})
    $record = [ordered]@{
        timestamp = [DateTimeOffset]::Now.ToString('o')
        trace_id = $trace
        event = $Event
        detail = $Detail
    }
    foreach ($key in $Fields.Keys) { $record[$key] = $Fields[$key] }
    $json = Protect-BuildLog ($record | ConvertTo-Json -Compress -Depth 8)
    [IO.File]::AppendAllText($auditFile, ($json + "`n"), $utf8)
}

function Protect-BuildLog {
    param([string]$Text)
    $Text = $Text -replace 'github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9]+', '[REDACTED]'
    $Text = $Text -replace '(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+', '$1[REDACTED]'
    return $Text -replace '(https?://)[^/\s:@]+:[^@\s/]+@', '$1[REDACTED]@'
}

function Set-BuildEnvironment {
    param([string]$Name, [AllowNull()][string]$Value)
    [Environment]::SetEnvironmentVariable($Name, $Value, 'Process')
}

function Invoke-Go {
    param([string[]]$Arguments, [string]$Phase)
    $PSNativeCommandUseErrorActionPreference = $false
    Write-BuildAudit 'go_started' '开始执行 Go 工具，不输出环境变量或凭据' @{ phase = $Phase; arguments = $Arguments }
    $output = @(& $goCommand @Arguments 2>&1)
    $code = $LASTEXITCODE
    foreach ($line in $output) { Write-Host (Protect-BuildLog ([string]$line)) }
    Write-BuildAudit 'go_completed' 'Go 命令执行完成' @{ phase = $Phase; exit_code = $code; output = ($output -join "`n") }
    if ($code -ne 0) { throw "Go 阶段 $Phase 失败，退出码 $code；详见 $auditFile" }
    return ,$output
}

function Get-SafeOutputDirectory {
    $path = if ([IO.Path]::IsPathRooted($OutputDir)) {
        [IO.Path]::GetFullPath($OutputDir)
    } else {
        [IO.Path]::GetFullPath((Join-Path $root $OutputDir))
    }
    $comparison = if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        [StringComparison]::OrdinalIgnoreCase
    } else { [StringComparison]::Ordinal }
    $prefix = $root.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    if (-not $path.StartsWith($prefix, $comparison)) {
        throw '输出目录必须位于项目目录内，不能使用项目根目录或项目外路径'
    }
    $cursor = $path
    while ($cursor.Length -ge $root.Length) {
        if (Test-Path -LiteralPath $cursor) {
            $item = Get-Item -LiteralPath $cursor -Force
            if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "输出目录及其父目录必须是普通目录，拒绝文件、链接或重解析点：$cursor"
            }
        }
        if ($cursor.Equals($root, $comparison)) { break }
        $cursor = [IO.Path]::GetDirectoryName($cursor)
    }
    return $path
}

$failure = $null
Push-Location -LiteralPath $root
try {
    Write-BuildAudit 'build_requested' '开始构建，默认检查代码，清理仅作用于受限输出文件' @{
        only = $Only; architecture = $Architecture; arm_version = $ArmVersion
        output_directory = $OutputDir; clean = [bool]$Clean; skip_tests = [bool]$SkipTests
    }
    $goCommand = Get-Command go -ErrorAction Stop
    $versionMatch = Select-String -LiteralPath (Join-Path $root 'main.go') -Pattern 'version\s*=\s*"([^"]+)"' | Select-Object -First 1
    if (-not $versionMatch) { throw '无法从 main.go 读取版本号' }
    $version = $versionMatch.Matches[0].Groups[1].Value
    $minimumGo = ((Get-Content -LiteralPath (Join-Path $root 'go.mod') | Where-Object { $_ -match '^go\s+' }) -replace '^go\s+', '').Trim()
    foreach ($key in $environmentNames) { Set-BuildEnvironment $key $null }
    Set-BuildEnvironment 'CGO_ENABLED' '0'
    $toolchain = (Invoke-Go @('version') 'toolchain') -join "`n"
    $toolVersion = [regex]::Match($toolchain, 'go(\d+\.\d+(?:\.\d+)?)')
    if ($toolVersion.Success -and ([version]$toolVersion.Groups[1].Value -lt [version]$minimumGo)) {
        throw "当前 Go 版本不足，go.mod 要求 Go $minimumGo 或更新工具链"
    }
    $supported = Invoke-Go @('tool', 'dist', 'list') 'supported_targets'
    $output = Get-SafeOutputDirectory

    $targets = @(
        @{ OS = 'windows'; Arch = '386'; Suffix = '386'; Ext = '.exe' }
        @{ OS = 'windows'; Arch = 'amd64'; Suffix = 'amd64'; Ext = '.exe' }
        @{ OS = 'windows'; Arch = 'arm64'; Suffix = 'arm64'; Ext = '.exe' }
        @{ OS = 'linux'; Arch = '386'; Suffix = '386'; Ext = '' }
        @{ OS = 'linux'; Arch = 'amd64'; Suffix = 'amd64'; Ext = '' }
        @{ OS = 'linux'; Arch = 'arm'; Suffix = "armv$ArmVersion"; Ext = '' }
        @{ OS = 'linux'; Arch = 'arm64'; Suffix = 'arm64'; Ext = '' }
        @{ OS = 'darwin'; Arch = 'amd64'; Suffix = 'amd64'; Ext = '' }
        @{ OS = 'darwin'; Arch = 'arm64'; Suffix = 'arm64'; Ext = '' }
    )
    if ($Only) { $targets = @($targets | Where-Object { $_.OS -eq $Only }) }
    if ($Architecture) { $targets = @($targets | Where-Object { $_.Arch -eq $Architecture }) }
    if ($targets.Count -eq 0) { throw "平台/架构组合不受支持：$Only/$Architecture（Windows ARM32 不支持）" }
    foreach ($target in $targets) {
        if ($supported -notcontains "$($target.OS)/$($target.Arch)") {
            throw "当前 Go 工具链不支持 $($target.OS)/$($target.Arch)"
        }
    }
    Write-Host "WorkBuddy Gateway v$version · $toolchain · go.mod 要求 Go $minimumGo"
    if (-not $SkipTests) {
        $null = Invoke-Go @('vet', './...') 'vet'
        $null = Invoke-Go @('test', './...') 'tests'
    } else {
        Write-BuildAudit 'checks_skipped' '用户指定 SkipTests，跳过代码检查' @{}
    }
    [IO.Directory]::CreateDirectory($output) | Out-Null
    if ($Clean) {
        $known = '^(workbuddy-gateway-(windows|linux|darwin)-(386|amd64|arm|arm64|armv[567])(\.exe)?|SHA256SUMS|build-manifest\.json|config\.example\.json)$'
        foreach ($file in Get-ChildItem -LiteralPath $output -File -Force) {
            if ($file.Name -notmatch $known) { continue }
            if ($file.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "拒绝清理链接产物：$($file.Name)" }
            Remove-Item -LiteralPath $file.FullName -Force
            Write-BuildAudit 'artifact_cleaned' '已清理一个已知构建文件，其他文件和子目录保留' @{ file = $file.Name }
        }
    }
    $artifacts = @()
    foreach ($target in $targets) {
        Set-BuildEnvironment 'GOOS' $target.OS
        Set-BuildEnvironment 'GOARCH' $target.Arch
        Set-BuildEnvironment 'GOARM' $(if ($target.Arch -eq 'arm') { $ArmVersion } else { $null })
        Set-BuildEnvironment 'GOAMD64' 'v1'
        Set-BuildEnvironment 'GO386' 'sse2'
        Set-BuildEnvironment 'GOARM64' 'v8.0'
        $name = "workbuddy-gateway-$($target.OS)-$($target.Suffix)$($target.Ext)"
        $path = Join-Path $output $name
        if (Test-Path -LiteralPath $path) {
            $existing = Get-Item -LiteralPath $path -Force
            if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "产物路径不是普通文件：$path"
            }
        }
        $temporary = Join-Path $output "$name.$trace.tmp"
        Write-Host "Building $($target.OS)/$($target.Arch) -> $name"
        try {
            $null = Invoke-Go @('build', '-trimpath', '-buildvcs=false', '-ldflags', '-s -w', '-o', $temporary, '.') "build_$($target.OS)_$($target.Arch)"
            if (-not (Test-Path -LiteralPath $temporary) -or (Get-Item -LiteralPath $temporary).Length -eq 0) { throw "没有有效产物：$name" }
            Move-Item -LiteralPath $temporary -Destination $path -Force
        } finally {
            if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
        }
        $artifacts += [ordered]@{
            name = $name; goos = $target.OS; goarch = $target.Arch
            goarm = $(if ($target.Arch -eq 'arm') { $ArmVersion } else { $null })
            bytes = (Get-Item -LiteralPath $path).Length
            sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        }
        Write-BuildAudit 'artifact_created' '产物完成并计算 SHA256' @{ artifact = $artifacts[-1] }
    }
    $examplePath = Join-Path $root 'config.example.json'
    foreach ($name in @('config.example.json', 'build-manifest.json', 'SHA256SUMS')) {
        $destination = Join-Path $output $name
        if (Test-Path -LiteralPath $destination) {
            $existing = Get-Item -LiteralPath $destination -Force
            if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "配置或清单输出路径不是普通文件：$destination"
            }
        }
    }
    $example = Get-Content -LiteralPath $examplePath -Raw | ConvertFrom-Json
    if ($example.gateway.adminKey -or $example.gateway.apiKey) { throw '示例配置包含密钥，拒绝复制或发布' }
    Copy-Item -LiteralPath $examplePath -Destination (Join-Path $output 'config.example.json') -Force
    $commit = 'unknown'
    $dirty = $null
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $commit = (& git rev-parse HEAD 2>$null | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) { $commit = 'unknown' }
        $dirty = [bool]((& git status --porcelain --untracked-files=no 2>$null | Out-String).Trim())
    }
    $manifest = [ordered]@{
        version = $version; source_commit = $commit; tracked_source_dirty = $dirty
        toolchain = $toolchain; minimum_go = $minimumGo
        built_at = [DateTimeOffset]::Now.ToString('o'); cgo_enabled = $false
        windows_arm32_supported = $false; artifacts = $artifacts
    }
    [IO.File]::WriteAllText((Join-Path $output 'build-manifest.json'), ($manifest | ConvertTo-Json -Depth 8) + "`n", $utf8)
    $sums = @($artifacts | ForEach-Object { "$($_.sha256)  $($_.name)" })
    foreach ($name in @('config.example.json', 'build-manifest.json')) {
        $hash = (Get-FileHash -LiteralPath (Join-Path $output $name) -Algorithm SHA256).Hash.ToLowerInvariant()
        $sums += "$hash  $name"
    }
    [IO.File]::WriteAllText((Join-Path $output 'SHA256SUMS'), ($sums -join "`n") + "`n", $utf8)
    Write-BuildAudit 'build_succeeded' '全部目标完成，构建清单与校验和已输出' @{ count = $artifacts.Count; output = $output; version = $version; commit = $commit }
    Write-Host "构建成功：$($artifacts.Count) 个二进制，输出 $output"
} catch {
    $failure = $_
    Write-BuildAudit 'build_failed' '构建失败，没有将本次结果标记为成功' @{ error = $_.Exception.Message }
} finally {
    foreach ($key in $environmentNames) { Set-BuildEnvironment $key $oldEnvironment[$key] }
    Pop-Location
}
if ($failure) { throw $failure }
