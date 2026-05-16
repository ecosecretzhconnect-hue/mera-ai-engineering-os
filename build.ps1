#Requires -Version 5.1
[CmdletBinding()]
param(
    [string]$Version = '',
    [string]$OutDir  = 'outputs',
    [switch]$SkipZip,
    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$enc = [System.Text.UTF8Encoding]::new($true)

function Write-Ok($m)   { Write-Host "[OK]  $m" -ForegroundColor Green }
function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Write-Fail($m) { Write-Host "[FAIL] $m" -ForegroundColor Red }

function Invoke-GenerateSampleApp {
    param([string]$dest)

    New-Item -ItemType Directory -Path $dest -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $dest 'Controllers') -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $dest 'Services') -Force | Out-Null

    $csproj = @(
        '<Project Sdk="Microsoft.NET.Sdk.Web">',
        '  <PropertyGroup>',
        '    <TargetFramework>net8.0</TargetFramework>',
        '    <Nullable>enable</Nullable>',
        '  </PropertyGroup>',
        '</Project>'
    ) -join "`n"
    [System.IO.File]::WriteAllText((Join-Path $dest 'MERA.SampleApp.csproj'), $csproj + "`n", $enc)

    $controller = @(
        'using Microsoft.AspNetCore.Mvc;',
        'using MERA.SampleApp.Services;',
        '',
        'namespace MERA.SampleApp.Controllers;',
        '',
        '[ApiController]',
        '[Route("[controller]")]',
        'public class AuthController : ControllerBase',
        '{',
        '    private readonly IAuthService _authService;',
        '',
        '    public AuthController(IAuthService authService)',
        '    {',
        '        _authService = authService;',
        '    }',
        '',
        '    [HttpPost("login")]',
        '    public IActionResult Login(LoginRequest req)',
        '    {',
        '        var token = _authService.Login(req.Username, req.Password);',
        '        return Ok(new { token });',
        '    }',
        '}',
        '',
        'public record LoginRequest(string Username, string Password);'
    ) -join "`n"
    [System.IO.File]::WriteAllText((Join-Path $dest 'Controllers\AuthController.cs'), $controller + "`n", $enc)

    $service = @(
        'namespace MERA.SampleApp.Services;',
        '',
        'public interface IAuthService',
        '{',
        '    string Login(string username, string password);',
        '}'
    ) -join "`n"
    [System.IO.File]::WriteAllText((Join-Path $dest 'Services\IAuthService.cs'), $service + "`n", $enc)

    $program = @(
        'using MERA.SampleApp.Services;',
        '',
        'var builder = WebApplication.CreateBuilder(args);',
        'builder.Services.AddControllers();',
        'builder.Services.AddScoped<IAuthService, StubAuthService>();',
        '',
        'var app = builder.Build();',
        'app.MapControllers();',
        'app.Run();',
        '',
        'public class StubAuthService : IAuthService',
        '{',
        '    public string Login(string username, string password) => "stub";',
        '}'
    ) -join "`n"
    [System.IO.File]::WriteAllText((Join-Path $dest 'Program.cs'), $program + "`n", $enc)

    Write-Ok 'MERA.SampleApp generated'
}

if ($Version -eq '') {
    $versionGo = Get-Content (Join-Path $PSScriptRoot 'version.go') -Raw -Encoding UTF8
    if ($versionGo -match '"(\d+\.\d+\.\d+)"') {
        $Version = $Matches[1]
    } else {
        $Version = '10.0.0'
    }
}

$BuildDate = (Get-Date -Format 'yyyy-MM-dd')
$GoArch    = 'amd64'
$GoOS      = 'windows'
$ZipName   = "mera-v$Version-$GoOS-$GoArch.zip"

Write-Host ''
Write-Host '+======================================================+' -ForegroundColor Cyan
Write-Host "|  MERA Build Pipeline  v$Version" -ForegroundColor Cyan
if ($DryRun) {
    Write-Host '|  DRY RUN - no files will be written' -ForegroundColor Yellow
}
Write-Host '+======================================================+' -ForegroundColor Cyan
Write-Host ''

$criticalFiles = @('setup.ps1', 'CHANGELOG.md', 'version.go')
foreach ($f in $criticalFiles) {
    $p = Join-Path $PSScriptRoot $f
    if (-not (Test-Path $p)) {
        Write-Fail "Critical file missing: $f"
        exit 1
    }
}
Write-Ok 'Pre-flight: critical files present'

if ($DryRun) {
    Write-Ok "DryRun complete - version resolved to $Version, all critical files present."
    exit 0
}

if (Test-Path $OutDir) {
    Write-Info "Cleaning output dir: $OutDir"
    Remove-Item (Join-Path $OutDir '*') -Recurse -Force -ErrorAction SilentlyContinue
} else {
    New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
}
Write-Info "Output dir: $OutDir"

$ExePath = Join-Path $OutDir 'mera.exe'
Write-Info 'Compiling mera.exe ...'

$ldflags = "-X main.BuildVersion=$Version -X main.BuildDate=$BuildDate -X main.InstallerVersion=$Version"
$env:GOOS   = $GoOS
$env:GOARCH = $GoArch

go build -ldflags $ldflags -o $ExePath .
if ($LASTEXITCODE -ne 0) {
    Write-Fail 'go build failed.'
    exit 1
}

Remove-Item Env:\GOOS   -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue

$exeMB = [Math]::Round((Get-Item $ExePath).Length / 1MB, 1)
Write-Ok "mera.exe compiled ($exeMB MB)"

Write-Info 'Verifying version consistency ...'
$verOutput = & $ExePath -Version 2>&1
$binaryVersion = ''

foreach ($line in $verOutput) {
    if ("$line" -match '^MERA\b.*?v(\d+\.\d+\.\d+)') {
        $binaryVersion = $Matches[1]
        break
    }
}

if ($binaryVersion -eq '') {
    foreach ($line in $verOutput) {
        if ("$line" -match '\bv(\d+\.\d+\.\d+)\b') {
            $binaryVersion = $Matches[1]
            break
        }
    }
}

if ($binaryVersion -eq '') {
    Write-Fail 'Could not parse version from mera.exe -Version output.'
    Write-Host '       Output was:'
    $verOutput | ForEach-Object { Write-Host "         $_" }
    exit 1
}

if ($binaryVersion -ne $Version) {
    Write-Fail "Version mismatch: build script=$Version, binary reports=$binaryVersion"
    exit 1
}

Write-Ok "Version consistent: $Version"

$verOutput = $null
Start-Sleep -Milliseconds 750
[GC]::Collect()
[GC]::WaitForPendingFinalizers()

foreach ($f in @('setup.ps1', 'CHANGELOG.md')) {
    $src = Join-Path $PSScriptRoot $f
    if (Test-Path $src) {
        Copy-Item $src (Join-Path $OutDir $f) -Force
        Write-Ok "Copied $f"
    } else {
        Write-Fail "Required file not found: $f"
        exit 1
    }
}

$verLines = @(
    "MERA Go v$Version",
    "Build date : $BuildDate",
    "Platform   : $GoOS/$GoArch"
) -join "`n"
[System.IO.File]::WriteAllText((Join-Path $OutDir 'VERSION.txt'), $verLines + "`n", $enc)
Write-Ok 'VERSION.txt'

$readme = @(
    "# MERA Go v$Version",
    '',
    'AI Engineering Operating System for Windows.',
    '',
    '## Quick Start',
    '',
    '```powershell',
    'powershell -ExecutionPolicy Bypass -File .\setup.ps1',
    '```',
    '',
    '## Commands',
    '',
    '| Command | Description |',
    '|---------|-------------|',
    '| `mera -Doctor` | System health check |',
    '| `mera -Init` | Init .mera in current project |',
    '| `mera -DryRun <module> "task"` | Analysis without code changes |',
    '| `mera -Code <module> "task"` | Full AI-assisted implementation |',
    '| `mera -Health` | Weighted health score |',
    '| `mera -SmokeTest` | Full installation smoke test |',
    '| `mera -Replay <session-id>` | Replay a past workflow session |',
    '| `mera -Sessions` | List all stored sessions |',
    '| `mera -Version` | Show version and build info |',
    '',
    'See CHANGELOG.md for release notes.',
    ''
) -join "`n"
[System.IO.File]::WriteAllText((Join-Path $OutDir 'README.md'), $readme, $enc)
Write-Ok 'README.md'

$sampleDst = Join-Path $OutDir 'MERA.SampleApp'
if (Test-Path $sampleDst) {
    Remove-Item $sampleDst -Recurse -Force -ErrorAction SilentlyContinue
}
New-Item -ItemType Directory -Path $sampleDst -Force | Out-Null

$sampleSrc = Join-Path $PSScriptRoot 'MERA.SampleApp'
if (Test-Path $sampleSrc) {
    Copy-Item "$sampleSrc\*" $sampleDst -Recurse -Force
    Write-Ok 'MERA.SampleApp copied'
} else {
    Write-Info 'Generating minimal MERA.SampleApp ...'
    Invoke-GenerateSampleApp $sampleDst
}

Write-Info 'Computing checksums ...'
$sumsLines = [System.Collections.Generic.List[string]]::new()
$checksums = @{}

foreach ($file in Get-ChildItem $OutDir -File) {
    $hash = (Get-FileHash $file.FullName -Algorithm SHA256).Hash.ToLower()
    $sumsLines.Add("$hash  $($file.Name)") | Out-Null
    $checksums[$file.Name] = $hash
    Write-Host "         $hash  $($file.Name)"
}

$sumsPath = Join-Path $OutDir 'SHA256SUMS.txt'
[System.IO.File]::WriteAllLines($sumsPath, $sumsLines, $enc)
Write-Ok 'SHA256SUMS.txt'

$fileNames = @()
foreach ($l in $sumsLines) {
    $fileNames += ($l -split '  ')[1]
}

$manifest = [ordered]@{
    manifestSchemaVersion = 1
    version              = $Version
    releaseDate          = $BuildDate
    files                = $fileNames
    checksums            = $checksums
    requiredConfigSchema = 1
    requiredModelsSchema = 1
    minInstallerVersion  = $Version
}
$manifestJson = $manifest | ConvertTo-Json -Depth 4
[System.IO.File]::WriteAllText((Join-Path $OutDir 'manifest.json'), $manifestJson + "`n", $enc)
Write-Ok 'manifest.json'

if (-not $SkipZip) {
    $parentDir = Split-Path $OutDir -Parent
    if ($parentDir -eq '') { $parentDir = '.' }

    $zipPath = Join-Path $parentDir $ZipName
    if (Test-Path $zipPath) {
        Remove-Item $zipPath -Force -ErrorAction SilentlyContinue
    }

    Write-Info "Creating $ZipName ..."

    $zipCreated = $false

    for ($i = 1; $i -le 5; $i++) {
        try {
            Start-Sleep -Seconds 2
            [GC]::Collect()
            [GC]::WaitForPendingFinalizers()

            Compress-Archive `
                -Path (Join-Path $OutDir '*') `
                -DestinationPath $zipPath `
                -Force `
                -ErrorAction Stop

            $zipCreated = $true
            break
        }
        catch {
            Write-Warn "Zip attempt $i failed: $($_.Exception.Message)"
            Start-Sleep -Seconds 3
        }
    }

    if (-not $zipCreated) {
        Write-Fail 'ZIP creation failed after 5 attempts.'
        Write-Info 'Try running with -SkipZip, or close antivirus/file indexing tools and retry.'
        exit 1
    }

    if (-not (Test-Path $zipPath)) {
        Write-Fail "ZIP was reported created but file is missing: $zipPath"
        exit 1
    }

    $zipMB = [Math]::Round((Get-Item $zipPath).Length / 1MB, 1)
    Write-Ok "$ZipName ($zipMB MB)"
}

Write-Host ''
Write-Host '+======================================================+' -ForegroundColor Green
Write-Host "|  MERA v$Version build complete" -ForegroundColor Green
Write-Host '+======================================================+' -ForegroundColor Green
Write-Host "  Output  : $OutDir"
if (-not $SkipZip) {
    Write-Host "  Archive : $ZipName"
}
Write-Host ''