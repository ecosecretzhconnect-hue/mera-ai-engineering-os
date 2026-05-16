#Requires -Version 5.1
<#
.SYNOPSIS
    MERA AI Engineering OS - Enterprise installer for Windows.

.DESCRIPTION
    Detects hardware, installs Python/Ollama/Aider, pulls AI models,
    generates MERA configs, and validates the full installation.
    Designed for zero-friction setup on any Windows machine.

.PARAMETER Minimal
    Install minimal model set - phi4 + qwen2.5-coder:7b (~10 GB).
.PARAMETER Fast
    Install fast model set - adds llama3.1:8b (~15 GB).
.PARAMETER Balanced
    Install balanced model set - adds qwen2.5-coder:14b (~25 GB).
.PARAMETER Deep
    Install full model set - adds deepseek-coder-v2 (~45 GB).
.PARAMETER Silent
    Non-interactive - accepts Balanced defaults without prompting.
.PARAMETER Update
    Update MERA binary and config schema while preserving all history.
.PARAMETER Uninstall
    Remove MERA binaries and configs with optional model cleanup.
.PARAMETER Repair
    Check and fix PATH, configs, tools, and missing models without reinstalling.
.PARAMETER UpgradeProfile
    Upgrade an existing MERA installation to a new model profile (Minimal/Fast/Balanced/Deep).
    Pulls missing models, rewrites models.json, and validates with mera -Doctor and mera -Health.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\setup.ps1
    powershell -ExecutionPolicy Bypass -File .\setup.ps1 -Minimal -Silent
    powershell -ExecutionPolicy Bypass -File .\setup.ps1 -Update
    powershell -ExecutionPolicy Bypass -File .\setup.ps1 -Repair
    powershell -ExecutionPolicy Bypass -File .\setup.ps1 -UpgradeProfile Balanced
    powershell -ExecutionPolicy Bypass -File .\setup.ps1 -UpgradeProfile Deep
#>
[CmdletBinding(DefaultParameterSetName = 'Install')]
param(
    [Parameter(ParameterSetName = 'Install')]   [switch]$Minimal,
    [Parameter(ParameterSetName = 'Install')]   [switch]$Fast,
    [Parameter(ParameterSetName = 'Install')]   [switch]$Balanced,
    [Parameter(ParameterSetName = 'Install')]   [switch]$Deep,
    [Parameter(ParameterSetName = 'Install')]   [switch]$Silent,
    [Parameter(ParameterSetName = 'Update')]    [switch]$Update,
    [Parameter(ParameterSetName = 'Uninstall')] [switch]$Uninstall,
    [Parameter(ParameterSetName = 'Repair')]    [switch]$Repair,
    [Parameter(ParameterSetName = 'Upgrade')]   [string]$UpgradeProfile = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Continue'
$ProgressPreference    = 'SilentlyContinue'   # suppress Invoke-WebRequest progress bars

# ── Constants ─────────────────────────────────────────────────────────────────

$MERA_VERSION  = 'v10.0.0'
$MERA_INSTALL  = 'C:\Tools\MERA'
$MERA_EXE_DST  = Join-Path $MERA_INSTALL 'mera.exe'
$MERA_EXE_SRC  = Join-Path $PSScriptRoot 'mera.exe'
$REPORT_FILE   = Join-Path $MERA_INSTALL 'MERA_INSTALL_REPORT.txt'
$LOG_FILE      = Join-Path $MERA_INSTALL 'install.log'
$OLLAMA_API    = 'http://localhost:11434/api/tags'
$MIN_DISK_GB   = 20
$MIN_RAM_GB    = 8
$REC_DEEP_GB   = 32

# Accumulated state (script-scoped, readable from all functions)
$Script:Warnings  = [System.Collections.Generic.List[string]]::new()
$Script:Installed = [System.Collections.Generic.List[string]]::new()
$Script:Skipped   = [System.Collections.Generic.List[string]]::new()
$Script:Failed    = [System.Collections.Generic.List[string]]::new()
$Script:RamGB     = 0
$Script:FreeDisk  = 0
$Script:HasNet    = $false
$Script:PyCmd     = $null
$Script:ToolFound = @{}

# ── Logging helpers ───────────────────────────────────────────────────────────

function Write-Ok   { param([string]$m) Write-Host "[OK]   $m" -ForegroundColor Green;   Append-Log "[OK]   $m" }
function Write-Warn { param([string]$m) Write-Host "[WARN] $m" -ForegroundColor Yellow;  Append-Log "[WARN] $m"; $Script:Warnings.Add($m) | Out-Null }
function Write-Fail { param([string]$m) Write-Host "[FAIL] $m" -ForegroundColor Red;     Append-Log "[FAIL] $m"; $Script:Failed.Add($m) | Out-Null }
function Write-Info { param([string]$m) Write-Host "[INFO] $m" -ForegroundColor Cyan;    Append-Log "[INFO] $m" }
function Write-Sep  { Write-Host ('=' * 56) -ForegroundColor DarkGray }
function Write-Head {
    param([string]$m)
    Write-Host ''
    Write-Sep
    Write-Host "  $m" -ForegroundColor White
    Write-Sep
    Append-Log "=== $m ==="
}

function Append-Log {
    param([string]$m)
    $line = "[$(Get-Date -Format 'HH:mm:ss')] $m"
    if (Test-Path $MERA_INSTALL) {
        try { Add-Content -Path $LOG_FILE -Value $line -Encoding UTF8 -ErrorAction SilentlyContinue } catch {}
    }
}

function Prompt-YN {
    param([string]$q)
    if ($Silent) { return $true }
    $ans = Read-Host "$q [Y/n]"
    return ($ans -eq '' -or $ans -match '^[Yy]$')
}

function Prompt-Int {
    param([string]$q, [int]$max)
    if ($Silent) { return 3 }   # default: Balanced
    $n = 0
    do {
        $raw = Read-Host $q
        [int]::TryParse($raw.Trim(), [ref]$n) | Out-Null
    } while ($n -lt 1 -or $n -gt $max)
    return $n
}

# ── Environment detection ─────────────────────────────────────────────────────

function Invoke-SysInfo {
    Write-Head 'Environment Detection'

    $os = Get-CimInstance Win32_OperatingSystem -ErrorAction SilentlyContinue
    if ($os) { Write-Info "OS   : $($os.Caption) build $($os.BuildNumber)" }

    $arch = if ([System.Environment]::Is64BitOperatingSystem) { 'x64' } else { 'x86' }
    Write-Info "Arch : $arch"

    # RAM
    if ($os) {
        $Script:RamGB = [math]::Round($os.TotalVisibleMemorySize / 1MB, 1)
        if ($Script:RamGB -lt $MIN_RAM_GB) {
            Write-Warn "RAM  : $($Script:RamGB) GB — below $MIN_RAM_GB GB; use Minimal profile"
        } else {
            Write-Ok   "RAM  : $($Script:RamGB) GB"
        }
    }

    # CPU
    $cpu = Get-CimInstance Win32_Processor -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($cpu) { Write-Info "CPU  : $($cpu.Name.Trim())" }

    # GPU
    $gpu = Get-CimInstance Win32_VideoController -ErrorAction SilentlyContinue |
           Where-Object { $_.Name -notmatch 'Microsoft Remote|Basic Display' } |
           Select-Object -First 1
    if ($gpu) { Write-Info "GPU  : $($gpu.Name)" }
    else       { Write-Warn "GPU  : none detected — CPU inference only (slower)" }

    # Disk
    $drive = Get-PSDrive C -ErrorAction SilentlyContinue
    if ($drive) {
        $Script:FreeDisk = [math]::Round($drive.Free / 1GB, 1)
        if ($Script:FreeDisk -lt $MIN_DISK_GB) {
            Write-Warn "Disk : $($Script:FreeDisk) GB free — at least $MIN_DISK_GB GB recommended"
        } else {
            Write-Ok   "Disk : $($Script:FreeDisk) GB free on C:"
        }
    }

    # Internet
    Test-InternetConnectivity | Out-Null
    if ($Script:HasNet) { Write-Ok   'Net  : internet reachable' }
    else                { Write-Warn 'Net  : internet not reachable — model downloads will fail' }
}

function Invoke-ToolDetect {
    Write-Head 'Tool Detection'

    $checks = @(
        @{ name = 'git';    cmd = 'git';    arg = '--version' }
        @{ name = 'go';     cmd = 'go';     arg = 'version' }
        @{ name = 'aider';  cmd = 'aider';  arg = '--version' }
        @{ name = 'ollama'; cmd = 'ollama'; arg = '--version' }
        @{ name = 'dotnet'; cmd = 'dotnet'; arg = '--version' }
        @{ name = 'node';   cmd = 'node';   arg = '--version' }
        @{ name = 'npm';    cmd = 'npm';    arg = '--version' }
        @{ name = 'docker'; cmd = 'docker'; arg = '--version' }
        @{ name = 'code';   cmd = 'code';   arg = '--version' }
    )

    foreach ($t in $checks) {
        $found = $null
        try { $found = Get-Command $t.cmd -ErrorAction SilentlyContinue } catch {}
        if ($found) {
            try {
                $ver = (& $t.cmd $t.arg 2>&1 | Select-Object -First 1)
                Write-Ok "$($t.name.PadRight(8)): $ver"
            } catch {
                Write-Ok "$($t.name.PadRight(8)): found"
            }
            $Script:ToolFound[$t.name] = $true
        } else {
            $required = $t.name -in @('aider','ollama')
            if ($required) {
                Write-Host "[NEED] $($t.name.PadRight(8)): not found — will install" -ForegroundColor Magenta
            } else {
                Write-Warn "$($t.name.PadRight(8)): not found (optional)"
            }
            $Script:ToolFound[$t.name] = $false
        }
    }

    Invoke-PythonDetect
}

function Invoke-PythonDetect {
    $Script:PyCmd = $null
    $storeDir     = Join-Path $env:LOCALAPPDATA 'Microsoft\WindowsApps'

    foreach ($candidate in @('python', 'python3', 'py')) {
        $found = $null
        try { $found = Get-Command $candidate -ErrorAction SilentlyContinue } catch {}
        if (-not $found) { continue }

        # Skip Microsoft Store redirect aliases
        if ($found.Source -and $found.Source.StartsWith($storeDir)) {
            Write-Warn "python  : Store alias at $($found.Source) — bypassing"
            continue
        }

        try {
            $ver = & $candidate --version 2>&1
            if ("$ver" -match 'Python 3\.\d+') {
                Write-Ok   "python  : $ver (via '$candidate')"
                $Script:PyCmd = $candidate
                $Script:ToolFound['python'] = $true
                return
            }
        } catch {}
    }

    Write-Host '[NEED] python  : not found or Store alias only — will install' -ForegroundColor Magenta
    $Script:ToolFound['python'] = $false
}

# ── Profile selection ─────────────────────────────────────────────────────────

function Get-HwProfile {
    if ($Script:RamGB -lt 12)       { return 'Minimal' }
    if ($Script:RamGB -lt 20)       { return 'Fast' }
    if ($Script:RamGB -lt $REC_DEEP_GB) { return 'Balanced' }
    return 'Deep'
}

$PROFILE_MODELS = @{
    Minimal  = @('qwen2.5-coder:7b')
    Fast     = @('phi4', 'qwen2.5-coder:7b', 'llama3.1:8b')
    Balanced = @('phi4', 'llama3.1:8b', 'qwen2.5-coder:14b')
    Deep     = @('phi4', 'llama3.1:8b', 'qwen2.5-coder:14b')
}

# Optional models: pulled with a warning on failure, not a hard requirement.
$PROFILE_OPTIONAL_MODELS = @{
    Minimal  = @()
    Fast     = @()
    Balanced = @()
    Deep     = @('deepseek-coder-v2')
}

# Approximate compressed download size per model (GB).
$MODEL_SIZE_GB = @{
    'qwen2.5-coder:7b'  = 5
    'phi4'              = 9
    'llama3.1:8b'       = 5
    'qwen2.5-coder:14b' = 9
    'deepseek-coder-v2' = 9
}

$PROFILE_DISK = @{
    Minimal = '~5 GB'; Fast = '~19 GB'; Balanced = '~23 GB'; Deep = '~32 GB (+9 GB optional deepseek)'
}

# Per-role model assignments keyed by install profile.
# 'meraProfile' sets the MERA performance profile (FAST/NORMAL/DEEP).
$PROFILE_ROUTER = @{
    Minimal = @{
        # All roles use the only available model on a Minimal install.
        planner = 'qwen2.5-coder:7b'; architect = 'qwen2.5-coder:7b'; filescout = 'qwen2.5-coder:7b'
        code = 'qwen2.5-coder:7b'; diffreview = 'qwen2.5-coder:7b'
        security = 'qwen2.5-coder:7b'; sprintadvisor = 'qwen2.5-coder:7b'; meraProfile = 'FAST'
    }
    Fast = @{
        planner = 'phi4'; architect = 'llama3.1:8b'; filescout = 'phi4'
        code = 'qwen2.5-coder:7b'; diffreview = 'llama3.1:8b'
        security = 'llama3.1:8b'; sprintadvisor = 'phi4'; meraProfile = 'FAST'
    }
    Balanced = @{
        planner = 'phi4'; architect = 'llama3.1:8b'; filescout = 'phi4'
        code = 'qwen2.5-coder:14b'; diffreview = 'llama3.1:8b'
        security = 'llama3.1:8b'; sprintadvisor = 'phi4'; meraProfile = 'NORMAL'
    }
    Deep = @{
        planner = 'phi4'; architect = 'llama3.1:8b'; filescout = 'phi4'
        code = 'qwen2.5-coder:14b'; diffreview = 'deepseek-coder-v2'
        security = 'llama3.1:8b'; sprintadvisor = 'phi4'; meraProfile = 'DEEP'
    }
}

function Select-Profile {
    # CLI flags take priority
    if ($Minimal)              { return 'Minimal' }
    if ($Fast)                 { return 'Fast' }
    if ($Deep)                 { return 'Deep' }
    if ($Balanced -or $Silent) { return 'Balanced' }

    Write-Head 'Model Profile Selection'

    $rec = Get-HwProfile
    Write-Info "Hardware recommendation: $rec  (RAM: $($Script:RamGB) GB, Disk: $($Script:FreeDisk) GB free)"
    if ($Script:RamGB -lt $MIN_RAM_GB)  { Write-Warn 'Low RAM — Deep/Balanced may be very slow.' }
    if ($Script:FreeDisk -lt 25)        { Write-Warn 'Limited disk space — Balanced or Minimal recommended.' }

    Write-Host ''
    Write-Host '  #  Profile    Disk      Models' -ForegroundColor White
    Write-Host '  -----------------------------------------------------------' -ForegroundColor DarkGray
    Write-Host '  1  Minimal    ~5 GB     qwen2.5-coder:7b  (one model, all roles)'
    Write-Host '  2  Fast       ~19 GB    + phi4, llama3.1:8b'
    Write-Host "  3  Balanced   ~23 GB    + qwen2.5-coder:14b  (rec: $rec)"
    Write-Host '  4  Deep       ~32 GB    + deepseek-coder-v2 (optional, +9 GB)'
    Write-Host ''

    $n = Prompt-Int 'Select profile (1-4)' 4
    return @('Minimal', 'Fast', 'Balanced', 'Deep')[$n - 1]
}

# ── winget helper ─────────────────────────────────────────────────────────────

function Test-Winget {
    $wg = Get-Command winget -ErrorAction SilentlyContinue
    if ($wg) { return $true }
    Write-Warn 'winget not available. Install "App Installer" from the Microsoft Store, then re-run setup.'
    return $false
}

function Invoke-Winget {
    param([string]$id, [string]$label)
    Write-Info "Installing $label via winget ($id)..."
    try {
        winget install --id $id --accept-source-agreements --accept-package-agreements --silent 2>&1 |
            ForEach-Object { Append-Log "  winget: $_" }
        Write-Ok   "$label installed."
        $Script:Installed.Add($label) | Out-Null
        return $true
    } catch {
        Write-Fail "winget install $id failed: $_"
        return $false
    }
}

# ── Python installation ───────────────────────────────────────────────────────

function Install-Python {
    if ($Script:ToolFound['python']) { return }
    Write-Head 'Python Installation'
    if (-not (Test-Winget)) { return }

    # Disable Store aliases before installing
    $storeDir = Join-Path $env:LOCALAPPDATA 'Microsoft\WindowsApps'
    foreach ($alias in @('python.exe', 'python3.exe')) {
        $target = Join-Path $storeDir $alias
        if (Test-Path $target) {
            try {
                Rename-Item $target "$target.disabled" -ErrorAction SilentlyContinue
                Write-Ok "Store alias disabled: $target"
            } catch {
                Write-Warn "Could not disable Store alias $target — may interfere"
            }
        }
    }

    $ok = Invoke-Winget 'Python.Python.3.12' 'Python 3.12'
    if (-not $ok) { return }

    Invoke-PathRefresh
    Invoke-PythonDetect
    if (-not $Script:PyCmd) {
        Write-Warn 'Python installed but not yet in PATH — open a new terminal if needed.'
    }
}

# ── Ollama installation ───────────────────────────────────────────────────────

function Install-Ollama {
    if ($Script:ToolFound['ollama']) {
        Write-Ok 'Ollama already installed — skipping.'
        $Script:Skipped.Add('Ollama') | Out-Null
        return
    }
    Write-Head 'Ollama Installation'
    if (-not (Test-Winget)) { return }
    Invoke-Winget 'Ollama.Ollama' 'Ollama' | Out-Null
    Invoke-PathRefresh
}

# ── Aider installation ────────────────────────────────────────────────────────

function Install-Aider {
    Write-Head 'Aider Installation'

    if ($Script:ToolFound['aider']) {
        Write-Ok 'Aider already installed — skipping.'
        $Script:Skipped.Add('Aider') | Out-Null
        return
    }
    if (-not $Script:PyCmd) {
        Write-Warn 'Python not available — skipping Aider. Install Python then run: pip install aider-chat'
        $Script:Failed.Add('Aider (Python missing)') | Out-Null
        return
    }

    Write-Info 'Installing aider-chat via pip (may take a few minutes)...'
    try {
        & $Script:PyCmd -m pip install --upgrade --quiet aider-chat 2>&1 |
            ForEach-Object { Append-Log "  pip: $_" }
        Invoke-PathRefresh
        $aider = Get-Command aider -ErrorAction SilentlyContinue
        if ($aider) {
            Write-Ok   'Aider installed.'
            $Script:Installed.Add('Aider') | Out-Null
        } else {
            Write-Warn 'Aider installed but not in PATH yet — open a new terminal.'
            $Script:Installed.Add('Aider (PATH refresh needed)') | Out-Null
        }
    } catch {
        Write-Fail   "pip install aider-chat failed: $_"
        $Script:Failed.Add('Aider') | Out-Null
    }
}

# ── Ollama server lifecycle ───────────────────────────────────────────────────

function Test-OllamaApi {
    try {
        $resp = Invoke-RestMethod $OLLAMA_API -TimeoutSec 3 -ErrorAction Stop
        return ($null -ne $resp)
    } catch {
        return $false
    }
}

function Start-OllamaServe {
    if (Test-OllamaApi) {
        Write-Ok 'Ollama API already reachable.'
        return $true
    }

    Write-Info 'Starting Ollama server...'
    try {
        Start-Process 'cmd.exe' -ArgumentList '/c ollama serve' -WindowStyle Minimized -ErrorAction Stop
    } catch {
        Write-Fail "Could not start ollama serve: $_"
        return $false
    }

    Write-Info 'Waiting for Ollama API (up to 60s)...'
    for ($i = 1; $i -le 30; $i++) {
        Start-Sleep -Seconds 2
        if (Test-OllamaApi) {
            Write-Ok "Ollama API ready ($($i * 2)s)."
            return $true
        }
        Write-Host '.' -NoNewline
    }
    Write-Host ''
    Write-Fail 'Ollama API did not respond after 60s.'
    return $false
}

function Get-InstalledModels {
    try {
        $resp = Invoke-RestMethod $OLLAMA_API -TimeoutSec 5 -ErrorAction Stop
        if ($null -eq $resp -or $null -eq $resp.models) { return @() }
        $names = @()
        foreach ($m in $resp.models) { $names += $m.name }
        return $names
    } catch {
        return @()
    }
}

# ── Internet connectivity ─────────────────────────────────────────────────────
# Separated from Invoke-SysInfo so Invoke-UpgradeProfile can call it independently.

function Test-InternetConnectivity {
    try {
        $p = Test-Connection '8.8.8.8' -Count 1 -Quiet -ErrorAction SilentlyContinue
        $Script:HasNet = [bool]$p
    } catch {
        $Script:HasNet = $false
    }
    return $Script:HasNet
}

# ── Model verification after pull ─────────────────────────────────────────────

# Returns a list of required model names that are NOT installed.
# Called after Install-Models to gate models.json rewriting.
function Get-MissingRequiredModels {
    param([string]$profileName)
    $installed = Get-InstalledModels
    $missing   = @()
    foreach ($m in $PROFILE_MODELS[$profileName]) {
        $found = $false
        foreach ($e in $installed) {
            if ($e -eq $m -or $e -like "$m`:*" -or
                ("$m" -notmatch ':' -and $e -eq "$m`:latest")) {
                $found = $true; break
            }
        }
        if (-not $found) { $missing += $m }
    }
    return $missing
}

# ── Disk protection ───────────────────────────────────────────────────────────

function Test-DiskForProfile {
    param([string]$profileName)

    # Only count disk space for models NOT already installed.
    $existing = @()
    if (Test-OllamaApi) { $existing = Get-InstalledModels }

    $neededNew = 0
    foreach ($m in $PROFILE_MODELS[$profileName]) {
        $alreadyHave = $false
        foreach ($e in $existing) {
            if ($e -eq $m -or $e -like "$m`:*" -or ("$m" -notmatch ':' -and $e -eq "$m`:latest")) {
                $alreadyHave = $true; break
            }
        }
        if (-not $alreadyHave) {
            $sz = $MODEL_SIZE_GB[$m]
            if ($null -ne $sz) { $neededNew += $sz }
        }
    }

    if ($neededNew -eq 0) {
        Write-Ok "All required $profileName models already installed — no download needed."
        return $true
    }

    $free = $Script:FreeDisk
    if ($free -lt 15) {
        Write-Warn "Low disk space: $free GB free. MERA recommends 15 GB minimum."
    }
    if ($free -lt $neededNew) {
        Write-Fail "Insufficient disk for $profileName profile."
        Write-Fail "  Need to download : ~$neededNew GB  |  Available : $free GB"
        if ($profileName -eq 'Deep')         { Write-Info 'Suggestion: .\setup.ps1 -Balanced' }
        elseif ($profileName -eq 'Balanced') { Write-Info 'Suggestion: .\setup.ps1 -Fast' }
        elseif ($profileName -ne 'Minimal')  { Write-Info 'Suggestion: .\setup.ps1 -Minimal  (~5 GB)' }
        return $false
    }
    Write-Ok "Disk OK: need ~$neededNew GB for new downloads, $free GB free."
    return $true
}

# ── Model installation ────────────────────────────────────────────────────────

function Invoke-ModelPull {
    param([string]$model, [bool]$optional = $false)
    $existing = Get-InstalledModels
    $alreadyHave = $false
    foreach ($e in $existing) {
        if ($e -eq $model -or $e -like "$model`:*" -or
            ("$model" -notmatch ':' -and $e -eq "$model`:latest")) {
            $alreadyHave = $true; break
        }
    }
    if ($alreadyHave) {
        Write-Ok "Model $model — already installed, skipping."
        $Script:Skipped.Add("model:$model") | Out-Null
        return
    }
    $label = if ($optional) { "optional model $model" } else { "model $model" }
    Write-Info "Pulling $label (this may take several minutes)..."
    try {
        $t0 = Get-Date
        ollama pull $model 2>&1 | ForEach-Object { Append-Log "  pull $model`: $_" }
        $elapsed = [math]::Round(((Get-Date) - $t0).TotalSeconds)
        Write-Ok "Model $model pulled ($($elapsed)s)."
        $Script:Installed.Add("model:$model") | Out-Null
    } catch {
        if ($optional) {
            Write-Warn "Optional model $model could not be pulled: $_"
            Write-Info "Pull manually later: ollama pull $model"
            $Script:Warnings.Add("optional model $model not pulled") | Out-Null
        } else {
            Write-Fail "Failed to pull $model`: $_"
            $Script:Failed.Add("model:$model") | Out-Null
        }
    }
}

function Install-Models {
    param([string]$profileName)
    Write-Head "Model Installation  [$profileName — $($PROFILE_DISK[$profileName])]"

    if (-not (Test-OllamaApi)) {
        Write-Warn 'Ollama API not reachable — skipping model installation.'
        $Script:Failed.Add('Models (Ollama offline)') | Out-Null
        return
    }
    if (-not $Script:HasNet) {
        Write-Warn 'No internet — cannot pull models. Pull manually with: ollama pull <model>'
        return
    }

    # Required models
    foreach ($model in $PROFILE_MODELS[$profileName]) {
        Invoke-ModelPull -model $model -optional $false
    }

    # Optional models (Deep profile: deepseek)
    $optionals = $PROFILE_OPTIONAL_MODELS[$profileName]
    if ($null -ne $optionals -and $optionals.Count -gt 0) {
        Write-Info "Optional models for $profileName profile:"
        foreach ($model in $optionals) {
            $optDiskGB = $MODEL_SIZE_GB[$model]
            if ($null -ne $optDiskGB -and $Script:FreeDisk -lt ($optDiskGB + 2)) {
                Write-Warn "Skipping optional $model — only $($Script:FreeDisk) GB free (~$optDiskGB GB needed)."
                continue
            }
            Invoke-ModelPull -model $model -optional $true
        }
    }
}

# ── Directory and file setup ──────────────────────────────────────────────────

function New-MeraDirectories {
    Write-Head 'Directory Setup'
    $dirs = @(
        $MERA_INSTALL
        (Join-Path $MERA_INSTALL '.mera')
        (Join-Path $MERA_INSTALL '.mera\reports')
        (Join-Path $MERA_INSTALL '.mera\snapshots')
        (Join-Path $MERA_INSTALL '.mera\sessions')
        (Join-Path $MERA_INSTALL '.mera\agents')
    )
    foreach ($d in $dirs) {
        if (-not (Test-Path $d)) {
            New-Item -ItemType Directory -Path $d -Force | Out-Null
            Write-Ok "Created: $d"
        } else {
            Write-Info "Exists : $d"
        }
    }
}

# Normalize a version string to exactly Major.Minor.Patch before comparison.
# Handles 1-part ("10"), 2-part ("10.0"), and 3-part ("10.0.1") inputs,
# with or without a leading "v".  Returns a [version] object.
# This is necessary because [version]"10.0" has Build=-1 and is therefore
# incorrectly treated as older than [version]"10.0.0" (Build=0) by PowerShell.
function ConvertTo-NormalizedVersion {
    param([string]$v)
    $v = $v.TrimStart('v').Trim()
    $parts = $v -split '\.'
    while ($parts.Count -lt 3) { $parts += '0' }
    return [version]($parts[0..2] -join '.')
}

function Get-Sha256 {
    param([string]$path)
    try {
        $hash = (Get-FileHash $path -Algorithm SHA256 -ErrorAction Stop).Hash.ToLower()
        return $hash
    } catch {
        return $null
    }
}

$Script:ManifestMalformed = $false

function Get-ReleaseManifest {
    $Script:ManifestMalformed = $false
    $manifestPath = Join-Path $PSScriptRoot 'manifest.json'
    if (-not (Test-Path $manifestPath)) { return $null }   # not found — caller checks $Silent
    try {
        $raw = Get-Content $manifestPath -Raw -Encoding UTF8 -ErrorAction Stop
        $obj = $raw | ConvertFrom-Json -ErrorAction Stop
        return $obj
    } catch {
        $Script:ManifestMalformed = $true
        return $null
    }
}

function Test-ExeChecksum {
    $manifest = Get-ReleaseManifest

    if ($Script:ManifestMalformed) {
        Write-Fail 'manifest.json is malformed — cannot verify mera.exe integrity.'
        exit 1
    }

    if ($null -eq $manifest) {
        if ($Silent) {
            Write-Fail 'Silent install requires manifest.json for checksum verification, but it was not found.'
            exit 1
        }
        Write-Info 'No manifest.json found — skipping checksum verification.'
        return
    }

    # Minimum installer version check.
    # Use ConvertTo-NormalizedVersion to pad both sides to Major.Minor.Patch
    # before comparing — [version]"10.0" has Build=-1 and is incorrectly
    # considered older than [version]"10.0.0" (Build=0) without normalization.
    $minVer = $null
    try { $minVer = $manifest.minInstallerVersion } catch {}
    if ($null -ne $minVer -and $minVer -ne '') {
        $installerVer = $MERA_VERSION.TrimStart('v')
        $installerVerNorm = ConvertTo-NormalizedVersion $installerVer
        $minVerNorm       = ConvertTo-NormalizedVersion $minVer
        if ($installerVerNorm -lt $minVerNorm) {
            Write-Fail "This installer (v$installerVer) is older than the minimum required (v$minVer)."
            Write-Fail 'Download the latest setup.ps1 from the release bundle.'
            exit 1
        }
    }

    # Checksum verification
    $expected = $null
    if ($null -ne $manifest.checksums) {
        try { $expected = $manifest.checksums.'mera.exe' } catch {}
    }
    if ($null -eq $expected -or $expected -eq '') {
        Write-Fail 'manifest.json does not contain a checksum for mera.exe — cannot verify integrity.'
        exit 1
    }

    Write-Info 'Verifying mera.exe checksum ...'
    $actual = Get-Sha256 $MERA_EXE_SRC
    if ($null -eq $actual) {
        Write-Fail 'Could not compute checksum for mera.exe — file may be locked or unreadable.'
        exit 1
    }
    if ($actual -eq $expected) {
        Write-Ok 'mera.exe checksum verified'
    } else {
        Write-Fail 'mera.exe checksum MISMATCH'
        Write-Fail "  Expected : $expected"
        Write-Fail "  Actual   : $actual"
        Write-Fail 'Install aborted — the binary does not match the manifest. Re-download the release bundle.'
        exit 1
    }
}

function Show-ReleaseInfo {
    $manifest = Get-ReleaseManifest
    if ($null -eq $manifest) { return }
    Write-Info "Release      : v$($manifest.version)  ($($manifest.releaseDate))"
    Write-Info "Config schema: v$($manifest.requiredConfigSchema)"
    Write-Info "Min installer: v$($manifest.minInstallerVersion)"
}

function Copy-MeraExe {
    if (-not (Test-Path $MERA_EXE_SRC)) {
        Write-Warn "mera.exe not found next to setup.ps1 — skipping binary copy."
        Write-Warn "Build from source: go build -o mera.exe ."
        $Script:Failed.Add('mera.exe (not found in bundle)') | Out-Null
        return
    }
    Test-ExeChecksum
    try {
        Copy-Item $MERA_EXE_SRC $MERA_EXE_DST -Force
        Write-Ok   "mera.exe installed: $MERA_EXE_DST"
        $Script:Installed.Add('mera.exe') | Out-Null
    } catch {
        Write-Fail  "Could not copy mera.exe: $_"
        $Script:Failed.Add('mera.exe (copy failed)') | Out-Null
    }
}

function Write-ModelsJson {
    param([string]$profileName)
    $router = $PROFILE_ROUTER[$profileName]
    # Build ordered hashtable matching the Go ModelConfig JSON schema exactly.
    # schemaVersion must be present or Go will trigger migration on every load.
    $obj = [ordered]@{
        schemaVersion = 1
        profile       = $router.meraProfile
        models        = [ordered]@{
            planner       = $router.planner
            architect     = $router.architect
            filescout     = $router.filescout
            code          = $router.code
            diffreview    = $router.diffreview
            security      = $router.security
            sprintadvisor = $router.sprintadvisor
        }
    }
    $path = Join-Path $MERA_INSTALL '.mera\models.json'
    # CRITICAL: use UTF-8 WITHOUT BOM — Go json.Unmarshal rejects BOM-prefixed files.
    # Set-Content -Encoding UTF8 adds BOM in PS 5.1; File.WriteAllText with UTF8Encoding($false) does not.
    $encNoBom = [System.Text.UTF8Encoding]::new($false)
    $json = $obj | ConvertTo-Json -Depth 4
    [System.IO.File]::WriteAllText($path, $json + "`n", $encNoBom)
    Write-Ok "models.json written  (install: $profileName  MERA profile: $($router.meraProfile))."
}

function Write-ConfigJson {
    $path = Join-Path $MERA_INSTALL '.mera\config.json'
    if (Test-Path $path) { Write-Info 'config.json exists — preserving user config.'; return }

    $cfg = [ordered]@{
        schemaVersion    = 1
        defaultModel     = 'qwen2.5-coder:7b'
        mapTokensFast    = 256
        mapTokensNormal  = 512
        mapTokensDeep    = 1024
        heartbeatSeconds = 15
        timeoutSeconds   = 600
        maxSessions      = 50
        autoCommit       = $false
        autoPush         = $false
        autoDeploy       = $false
        stack = [ordered]@{
            frontend = [ordered]@{
                framework = 'auto-detect'
                rules = @('Use existing components', 'No new deps without approval', 'Minimal scoped changes')
            }
            backend = [ordered]@{
                framework = 'auto-detect'
                rules = @('Thin controllers', 'No secrets in code', 'Do not change routes unless requested', 'Add tests for new logic')
            }
        }
        modules = [ordered]@{
            Identity = [ordered]@{ forbidden = @('Gateway/**', 'PaymentGateway/**') }
            Gateway  = [ordered]@{ forbidden = @('Identity/**', 'PaymentGateway/**') }
        }
    }
    # CRITICAL: write without BOM so Go json.Unmarshal can parse it.
    $encNoBom = [System.Text.UTF8Encoding]::new($false)
    $json = $cfg | ConvertTo-Json -Depth 8
    [System.IO.File]::WriteAllText($path, $json + "`n", $encNoBom)
    Write-Ok 'config.json written.'
}

function Write-AiderFiles {
    $confPath = Join-Path $MERA_INSTALL '.aider.conf.yml'
    if (-not (Test-Path $confPath)) {
        @('auto-commits: false', 'dirty-commits: false', 'show-diffs: true', 'pretty: true') -join "`n" |
            Set-Content $confPath -Encoding UTF8
        Write-Ok '.aider.conf.yml written.'
    } else {
        Write-Info '.aider.conf.yml exists — skipping.'
    }

    $ignorePath = Join-Path $MERA_INSTALL '.aiderignore'
    if (-not (Test-Path $ignorePath)) {
        @('.git', 'bin', 'obj', 'node_modules', '.next', 'dist', 'build',
          'coverage', '.vs', '.idea', '*.user', '*.suo', '*.cache',
          '.mera/logs', '.mera/reports') -join "`n" |
            Set-Content $ignorePath -Encoding UTF8
        Write-Ok '.aiderignore written.'
    } else {
        Write-Info '.aiderignore exists — skipping.'
    }

    $histPath = Join-Path $MERA_INSTALL '.mera\history.md'
    if (-not (Test-Path $histPath)) {
        '# MERA History' | Set-Content $histPath -Encoding UTF8
        Write-Ok '.mera/history.md created.'
    }
}

# ── PATH management ───────────────────────────────────────────────────────────

function Add-DirToPath {
    param([string]$dir)
    if (-not (Test-Path $dir)) { return }

    $current = [System.Environment]::GetEnvironmentVariable('PATH', 'Machine')
    if ($null -eq $current) { $current = '' }

    $parts = $current -split ';' | Where-Object { $_ -ne '' }
    if ($parts -contains $dir) { Write-Info "PATH has: $dir"; return }

    $newPath = ($parts + $dir) -join ';'
    try {
        [System.Environment]::SetEnvironmentVariable('PATH', $newPath, 'Machine')
        Write-Ok "Machine PATH += $dir"
    } catch {
        # Fallback to user PATH when not running as admin
        $userCurrent = [System.Environment]::GetEnvironmentVariable('PATH', 'User')
        if ($null -eq $userCurrent) { $userCurrent = '' }
        $userParts = $userCurrent -split ';' | Where-Object { $_ -ne '' }
        if ($userParts -notcontains $dir) {
            [System.Environment]::SetEnvironmentVariable('PATH', ($userParts + $dir) -join ';', 'User')
            Write-Ok "User PATH += $dir"
        }
    }
    # Refresh current session immediately
    if ($env:PATH -notmatch [regex]::Escape($dir)) { $env:PATH = "$env:PATH;$dir" }
}

function Invoke-PathSetup {
    Write-Head 'PATH Configuration'

    Add-DirToPath $MERA_INSTALL

    # Ollama default install location (winget)
    $ollamaDir = Join-Path $env:LOCALAPPDATA 'Programs\Ollama'
    Add-DirToPath $ollamaDir

    # Python paths (winget defaults)
    foreach ($p in @(
        (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python312'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python312\Scripts'),
        'C:\Python312', 'C:\Python312\Scripts'
    )) { Add-DirToPath $p }

    # Go
    foreach ($p in @('C:\Program Files\Go\bin', (Join-Path $env:USERPROFILE 'go\bin'))) {
        Add-DirToPath $p
    }
}

function Invoke-PathRefresh {
    $machine = [System.Environment]::GetEnvironmentVariable('PATH', 'Machine')
    $user    = [System.Environment]::GetEnvironmentVariable('PATH', 'User')
    if ($null -eq $machine) { $machine = '' }
    if ($null -eq $user)    { $user    = '' }
    $env:PATH = "$machine;$user"
}

function Test-MeraGlobal {
    Write-Head 'Global PATH Verification'
    Invoke-PathRefresh

    $meraCmd = Get-Command 'mera.exe' -ErrorAction SilentlyContinue
    if ($null -eq $meraCmd) {
        $meraCmd = Get-Command 'mera' -ErrorAction SilentlyContinue
    }

    if ($null -eq $meraCmd) {
        Write-Warn 'mera is not yet visible in the current session PATH.'
        Write-Warn 'Open a NEW terminal after setup completes — PATH changes take effect in new sessions.'
        Write-Info "Manual check: & '$MERA_EXE_DST' -Version"
        $Script:Warnings.Add('Open a new terminal for PATH to take effect') | Out-Null
        return
    }

    try {
        $verOut  = & $meraCmd.Source '-Version' 2>&1
        $verLine = $verOut | Where-Object { "$_" -match 'MERA' } | Select-Object -First 1
        if ($verLine) {
            Write-Ok "mera -Version: $verLine"
        } else {
            Write-Ok "mera found in PATH at: $($meraCmd.Source)"
        }
    } catch {
        Write-Warn "mera found in PATH but -Version failed: $_"
    }
}

# ── Self-validation ───────────────────────────────────────────────────────────

function Invoke-Validation {
    Write-Head 'Self-Validation'

    if (-not (Test-Path $MERA_EXE_DST)) {
        Write-Warn "mera.exe not found at $MERA_EXE_DST — skipping validation."
        return
    }

    Push-Location $MERA_INSTALL
    try {
        # 1. Version
        Write-Info 'mera -Version'
        $verOut  = & $MERA_EXE_DST '-Version' 2>&1
        $verLine = $verOut | Where-Object { "$_" -match 'MERA' } | Select-Object -First 1
        if ($verLine) {
            Write-Ok "  $verLine"
        } else {
            Write-Warn '  -Version produced no recognisable output'
        }

        # 2. Doctor (model routing + tool check)
        Write-Info 'mera -Doctor'
        & $MERA_EXE_DST '-Doctor' 2>&1 | ForEach-Object { Write-Host "  $_"; Append-Log "  [validate] $_" }

        # 3. Models
        Write-Info 'mera -Models'
        & $MERA_EXE_DST '-Models' 2>&1 | ForEach-Object { Write-Host "  $_"; Append-Log "  [validate] $_" }

        # 4. Health — extract score and gate
        Write-Info 'mera -Health'
        $healthOut = & $MERA_EXE_DST '-Health' 2>&1
        $healthOut | ForEach-Object { Write-Host "  $_"; Append-Log "  [validate] $_" }
        $scoreLine = $healthOut | Where-Object { "$_" -match 'HEALTH:\s*(\d+)%' } | Select-Object -First 1
        if ($scoreLine -match 'HEALTH:\s*(\d+)%') {
            $score = [int]$Matches[1]
            if ($score -ge 70) {
                Write-Ok "Health score: $score% — installation healthy"
            } elseif ($score -ge 50) {
                Write-Warn "Health score: $score% — some components degraded (run mera -Doctor)"
                $Script:Warnings.Add("Health score $score% — run mera -Doctor to investigate") | Out-Null
            } else {
                Write-Fail "Health score: $score% — installation has critical issues"
                $Script:Failed.Add("Health score: $score% (run mera -Doctor)") | Out-Null
            }
        }
    } catch {
        Write-Warn "Validation error: $_"
    } finally {
        Pop-Location
    }
}

# ── Install report ────────────────────────────────────────────────────────────

function Write-Report {
    param([string]$profileName)

    $lines = [System.Collections.Generic.List[string]]::new()
    $sep   = '=' * 56
    $lines.Add($sep)
    $lines.Add("  MERA $MERA_VERSION  Installation Report")
    $lines.Add("  $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')")
    $lines.Add($sep)
    $lines.Add('')
    $lines.Add("Profile    : $profileName")
    $lines.Add("Install dir: $MERA_INSTALL")
    $lines.Add("RAM        : $($Script:RamGB) GB")
    $lines.Add("Disk free  : $($Script:FreeDisk) GB")
    $lines.Add('')

    if ($Script:Installed.Count -gt 0) {
        $lines.Add('Installed:')
        foreach ($i in $Script:Installed) { $lines.Add("  + $i") }
        $lines.Add('')
    }
    if ($Script:Skipped.Count -gt 0) {
        $lines.Add('Skipped (already present):')
        foreach ($s in $Script:Skipped) { $lines.Add("  ~ $s") }
        $lines.Add('')
    }
    if ($Script:Warnings.Count -gt 0) {
        $lines.Add('Warnings:')
        foreach ($w in $Script:Warnings) { $lines.Add("  ! $w") }
        $lines.Add('')
    }
    if ($Script:Failed.Count -gt 0) {
        $lines.Add('Failures:')
        foreach ($f in $Script:Failed) { $lines.Add("  x $f") }
        $lines.Add('')
    }

    $lines.Add('Next commands:')
    $lines.Add("  cd $MERA_INSTALL")
    $lines.Add("  mera -Doctor           # verify all tools")
    $lines.Add("  mera -Models           # verify model routing")
    $lines.Add("  mera -Init             # initialise project in your repo")
    $lines.Add("  mera -DryRun <module> `"task`"")
    $lines.Add("  mera -Code   <module> `"task`"")
    $lines.Add('')
    $lines.Add('Model management:')
    $lines.Add("  mera -SetModel code phi4")
    $lines.Add("  mera -SetProfile FAST|NORMAL|DEEP|STRICT")
    $lines.Add("  mera -ProfileMode")
    $lines.Add($sep)

    $content = $lines -join "`n"
    try {
        Set-Content $REPORT_FILE -Value $content -Encoding UTF8
        Write-Ok "Install report: $REPORT_FILE"
    } catch {
        Write-Warn "Could not write report: $_"
    }
    Write-Host ''
    Write-Host $content -ForegroundColor Cyan
}

# ── Update mode ───────────────────────────────────────────────────────────────

function Invoke-Update {
    Write-Head 'MERA Update'

    # Show version info from manifest
    Show-ReleaseInfo

    # Detect installed version for upgrade notes
    $installedVersion = $null
    if (Test-Path $MERA_EXE_DST) {
        try {
            $verOutput = & $MERA_EXE_DST -Version 2>&1 | Select-String 'MERA Go'
            if ($verOutput -match 'v(\d+\.\d+\.\d+)') { $installedVersion = $Matches[1] }
        } catch {}
    }
    $manifest = Get-ReleaseManifest
    $newVersion = if ($null -ne $manifest) { $manifest.version } else { $MERA_VERSION }

    if ($null -ne $installedVersion) {
        Write-Info "Installed version : v$installedVersion"
        Write-Info "New version       : v$newVersion"
        if ($installedVersion -eq $newVersion) {
            Write-Info 'Already up to date. Re-installing anyway...'
        } else {
            Write-Info "Upgrading v$installedVersion -> v$newVersion"
            # Backup existing config before migration
            $cfgPath = Join-Path $MERA_INSTALL '.mera\config.json'
            if (Test-Path $cfgPath) {
                $bakPath = $cfgPath + ".bak.$installedVersion"
                Copy-Item $cfgPath $bakPath -Force
                Write-Ok "Config backup: $bakPath"
            }
        }
    }

    Copy-MeraExe

    # Merge new role defaults into existing models.json without overwriting user assignments
    $modelsPath = Join-Path $MERA_INSTALL '.mera\models.json'
    if (Test-Path $modelsPath) {
        try {
            $raw = Get-Content $modelsPath -Raw | ConvertFrom-Json
            $defaults = @{
                planner = 'phi4'; architect = 'llama3.1:8b'; filescout = 'phi4'
                code = 'qwen2.5-coder:14b'; diffreview = 'deepseek-coder-v2'
                security = 'llama3.1:8b'; sprintadvisor = 'phi4'
            }
            $changed = $false
            foreach ($role in $defaults.Keys) {
                $props = $raw.models.PSObject.Properties.Name
                if ($props -notcontains $role) {
                    $raw.models | Add-Member -NotePropertyName $role -NotePropertyValue $defaults[$role]
                    Write-Ok "Added missing role '$role' to models.json"
                    $changed = $true
                }
            }
            if ($changed) {
                # CRITICAL: BOM-free write so Go json.Unmarshal can parse it.
                $encNoBom = [System.Text.UTF8Encoding]::new($false)
                [System.IO.File]::WriteAllText($modelsPath, ($raw | ConvertTo-Json -Depth 5) + "`n", $encNoBom)
                Write-Ok 'models.json updated.'
            } else {
                Write-Info 'models.json is current — no changes needed.'
            }
        } catch {
            Write-Warn "Could not update models.json: $_"
        }
    }

    Invoke-PathSetup
    Write-Ok "Update complete. Run 'mera -Doctor' to verify."
}

# ── Uninstall mode ────────────────────────────────────────────────────────────

function Invoke-Uninstall {
    Write-Head 'MERA Uninstall'

    if (-not (Prompt-YN "Remove MERA binaries and config from $MERA_INSTALL?")) {
        Write-Info 'Uninstall cancelled.'
        return
    }

    if (Test-Path $MERA_EXE_DST) { Remove-Item $MERA_EXE_DST -Force; Write-Ok "Removed: $MERA_EXE_DST" }

    if (Prompt-YN 'Remove MERA config files? (history.md and profile.json will be preserved)') {
        foreach ($rel in @('.mera\models.json', '.mera\config.json', '.aider.conf.yml', '.aiderignore')) {
            $p = Join-Path $MERA_INSTALL $rel
            if (Test-Path $p) { Remove-Item $p -Force; Write-Ok "Removed: $p" }
        }
    }

    if ((Test-OllamaApi) -and (Prompt-YN 'Remove MERA AI models from Ollama?')) {
        foreach ($m in @('phi4', 'qwen2.5-coder:7b', 'qwen2.5-coder:14b', 'llama3.1:8b', 'deepseek-coder-v2')) {
            try {
                ollama rm $m 2>&1 | Out-Null
                Write-Ok "Removed model: $m"
            } catch {
                Write-Info "Model $m not installed."
            }
        }
    }

    Write-Ok 'Uninstall complete.'
}

# ── Checkpoint / resume ───────────────────────────────────────────────────────

$CHECKPOINT_FILE = Join-Path $MERA_INSTALL 'install.state.json'

function Get-Checkpoint {
    if (Test-Path $CHECKPOINT_FILE) {
        try {
            $raw = Get-Content $CHECKPOINT_FILE -Raw -Encoding UTF8
            $obj = $raw | ConvertFrom-Json
            # Convert PSCustomObject back to hashtable
            $ht = @{}
            $obj.PSObject.Properties | ForEach-Object { $ht[$_.Name] = $_.Value }
            return $ht
        } catch {}
    }
    return @{
        startedAt    = ''
        profile      = ''
        python       = $false
        ollama       = $false
        aider        = $false
        directories  = $false
        binary       = $false
        configFiles  = $false
        models       = $false
        path         = $false
        complete     = $false
    }
}

function Save-Checkpoint {
    param([hashtable]$cp)
    try {
        $cp | ConvertTo-Json -Depth 3 |
            Out-File $CHECKPOINT_FILE -Encoding UTF8 -Force
    } catch {}
}

function Clear-Checkpoint {
    if (Test-Path $CHECKPOINT_FILE) { Remove-Item $CHECKPOINT_FILE -Force -ErrorAction SilentlyContinue }
}

function Test-JsonFile {
    param([string]$path)
    if (-not (Test-Path $path)) { return $false }
    try {
        $raw = Get-Content $path -Raw -Encoding UTF8 -ErrorAction Stop
        $null = $raw | ConvertFrom-Json -ErrorAction Stop
        return $true
    } catch {
        return $false
    }
}

function Test-MeraPath {
    $machinePath = [System.Environment]::GetEnvironmentVariable('PATH', 'Machine')
    if ($null -eq $machinePath) { $machinePath = '' }
    return $machinePath.ToLower().Contains($MERA_INSTALL.ToLower())
}

# ── Repair mode ───────────────────────────────────────────────────────────────

function Invoke-Repair {
    Write-Head 'MERA Repair Mode'

    $fixed  = [System.Collections.Generic.List[string]]::new()
    $issues = [System.Collections.Generic.List[string]]::new()

    # 1. MERA install directory
    if (-not (Test-Path $MERA_INSTALL)) {
        New-Item -ItemType Directory -Path $MERA_INSTALL -Force | Out-Null
        $fixed.Add('Created missing MERA install directory') | Out-Null
        Write-Ok "Created $MERA_INSTALL"
    } else {
        Write-Ok "Install dir: $MERA_INSTALL"
    }

    # 2. MERA binary
    if (-not (Test-Path $MERA_EXE_DST)) {
        if (Test-Path $MERA_EXE_SRC) {
            Copy-Item $MERA_EXE_SRC $MERA_EXE_DST -Force
            $fixed.Add('Restored mera.exe from installer directory') | Out-Null
            Write-Ok 'mera.exe: restored'
        } else {
            $issues.Add('mera.exe missing — rerun setup.ps1 from the original installer directory') | Out-Null
            Write-Fail 'mera.exe: not found in install dir or script dir'
        }
    } else {
        Write-Ok 'mera.exe: present'
    }

    # 3. PATH
    if (Test-MeraPath) {
        Write-Ok "PATH: $MERA_INSTALL is already in Machine PATH"
    } else {
        try {
            $cur = [System.Environment]::GetEnvironmentVariable('PATH', 'Machine')
            if ($null -eq $cur) { $cur = '' }
            [System.Environment]::SetEnvironmentVariable('PATH', "$cur;$MERA_INSTALL", 'Machine')
            $fixed.Add("Added $MERA_INSTALL to Machine PATH") | Out-Null
            Write-Ok "PATH: added $MERA_INSTALL"
        } catch {
            $issues.Add("Could not set PATH — run as Administrator") | Out-Null
            Write-Fail "PATH: failed to update ($($_.Exception.Message))"
        }
    }

    # 4. config.json
    $cfgPath = Join-Path $MERA_INSTALL '.mera\config.json'
    if (Test-JsonFile $cfgPath) {
        Write-Ok 'config.json: valid'
    } else {
        Write-Warn 'config.json: missing or invalid — regenerating'
        Write-ConfigJson
        if (Test-JsonFile $cfgPath) {
            $fixed.Add('Regenerated config.json') | Out-Null
            Write-Ok 'config.json: regenerated'
        } else {
            $issues.Add('config.json could not be regenerated') | Out-Null
            Write-Fail 'config.json: regeneration failed'
        }
    }

    # 5. models.json
    $modPath = Join-Path $MERA_INSTALL '.mera\models.json'
    if (Test-JsonFile $modPath) {
        Write-Ok 'models.json: valid'
    } else {
        Write-Warn 'models.json: missing or invalid — regenerating'
        Write-ModelsJson 'Balanced'
        if (Test-JsonFile $modPath) {
            $fixed.Add('Regenerated models.json with Balanced profile') | Out-Null
            Write-Ok 'models.json: regenerated'
        } else {
            $issues.Add('models.json could not be regenerated') | Out-Null
            Write-Fail 'models.json: regeneration failed'
        }
    }

    # 6. Python
    $pyCmd = $null
    foreach ($try in @('python', 'python3', 'py')) {
        try {
            $v = & $try --version 2>&1
            if ($v -match 'Python 3') { $pyCmd = $try; break }
        } catch {}
    }
    if ($null -ne $pyCmd) {
        Write-Ok "Python: $pyCmd found"
    } else {
        $issues.Add('Python not found — run: winget install Python.Python.3.11') | Out-Null
        Write-Fail 'Python: not found — reinstall manually'
    }

    # 7. Aider
    $aiderPath = Get-Command 'aider' -ErrorAction SilentlyContinue
    if ($null -ne $aiderPath) {
        Write-Ok 'Aider: found'
    } else {
        Write-Warn 'Aider: not found — attempting pip install'
        if ($null -ne $pyCmd) {
            try {
                & $pyCmd -m pip install aider-chat --quiet 2>&1 | Out-Null
                $fixed.Add('Installed aider-chat via pip') | Out-Null
                Write-Ok 'Aider: installed'
            } catch {
                $issues.Add("Aider install failed: $($_.Exception.Message)") | Out-Null
                Write-Fail 'Aider: install failed'
            }
        } else {
            $issues.Add('Aider not installed and Python unavailable') | Out-Null
            Write-Fail 'Aider: cannot install without Python'
        }
    }

    # 8. Ollama API
    $ollamaUp = $false
    try {
        $r = Invoke-WebRequest -Uri $OLLAMA_API -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop
        $ollamaUp = ($r.StatusCode -eq 200)
    } catch {}
    if ($ollamaUp) {
        Write-Ok 'Ollama API: reachable'
    } else {
        Write-Warn 'Ollama API: not reachable — attempting to start Ollama'
        try {
            Start-Process 'ollama' -ArgumentList 'serve' -WindowStyle Hidden -ErrorAction Stop
            Start-Sleep -Seconds 4
            $r2 = Invoke-WebRequest -Uri $OLLAMA_API -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop
            if ($r2.StatusCode -eq 200) {
                $ollamaUp = $true
                $fixed.Add('Started Ollama serve') | Out-Null
                Write-Ok 'Ollama API: started'
            }
        } catch {
            $issues.Add("Ollama not reachable and could not be started") | Out-Null
            Write-Fail 'Ollama: could not start'
        }
    }

    # 9. Models
    if ($ollamaUp) {
        Repair-MissingModels
    } else {
        Write-Warn 'Skipping model check — Ollama not running'
    }

    Write-RepairReport $fixed $issues
}

function Repair-MissingModels {
    $modPath = Join-Path $MERA_INSTALL '.mera\models.json'
    if (-not (Test-JsonFile $modPath)) { return }

    $raw = Get-Content $modPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $neededSet = @{}
    $raw.models.PSObject.Properties | ForEach-Object { $neededSet[$_.Value] = $true }

    $installed = Get-InstalledModels

    $missing = @()
    foreach ($m in $neededSet.Keys) {
        $found = $false
        foreach ($inst in $installed) {
            if ($inst -eq $m -or $inst.StartsWith(($m -replace ':.*', '') + ':')) {
                $found = $true; break
            }
        }
        if (-not $found) { $missing += $m }
    }

    if ($missing.Count -eq 0) {
        Write-Ok 'Models: all configured models installed'
        return
    }

    Write-Warn "Models missing: $($missing -join ', ')"
    if (Prompt-YN "Pull $($missing.Count) missing model(s) now?") {
        foreach ($m in $missing) {
            Write-Info "Pulling $m ..."
            ollama pull $m
            Write-Ok "Pulled: $m"
        }
    } else {
        Write-Info 'Skipped model pull. Pull manually: ollama pull <model>'
    }
}

function Write-RepairReport {
    param(
        [System.Collections.Generic.List[string]]$Fixed,
        [System.Collections.Generic.List[string]]$Issues
    )
    $repPath = Join-Path $MERA_INSTALL 'MERA_REPAIR_REPORT.txt'
    $lines = @(
        "MERA Repair Report",
        "Generated: $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')",
        "",
        "Fixed ($($Fixed.Count)):"
    )
    if ($Fixed.Count -eq 0) { $lines += "  (nothing needed fixing)" }
    foreach ($f in $Fixed) { $lines += "  [FIXED] $f" }
    $lines += ""
    $lines += "Remaining issues ($($Issues.Count)):"
    if ($Issues.Count -eq 0) { $lines += "  (none)" }
    foreach ($i in $Issues) { $lines += "  [ISSUE] $i" }
    $lines += ""
    $lines += "Run 'mera -Doctor' to verify the full installation."

    $enc = [System.Text.UTF8Encoding]::new($true)
    [System.IO.File]::WriteAllLines($repPath, $lines, $enc)

    Write-Sep
    if ($Issues.Count -eq 0) {
        Write-Host '  Repair complete. No remaining issues.' -ForegroundColor Green
    } else {
        Write-Host "  Repair done. $($Issues.Count) issue(s) remain — see $repPath" -ForegroundColor Yellow
    }
    Write-Sep
    Write-Info "Report: $repPath"
}

# ── Minimal profile warning ───────────────────────────────────────────────────

function Show-MinimalWarning {
    Write-Host ''
    Write-Host '+------------------------------------------------------+' -ForegroundColor Yellow
    Write-Host '|  MERA Minimal Profile Installed                      |' -ForegroundColor Yellow
    Write-Host '+------------------------------------------------------+' -ForegroundColor Yellow
    Write-Host '|  Recommended for:                                    |' -ForegroundColor White
    Write-Host '|    mera -Doctor        mera -Health                  |' -ForegroundColor Green
    Write-Host '|    mera -Diag          mera -DryRun                  |' -ForegroundColor Green
    Write-Host '|    mera -ExplainSelection                            |' -ForegroundColor Green
    Write-Host '|  Not recommended for:                                |' -ForegroundColor White
    Write-Host '|    mera -Code  on repos with > 500 files             |' -ForegroundColor Red
    Write-Host '|  To enable full code execution:                      |' -ForegroundColor White
    Write-Host '|    .\setup.ps1 -UpgradeProfile Balanced              |' -ForegroundColor Cyan
    Write-Host '+------------------------------------------------------+' -ForegroundColor Yellow
    Write-Host ''
}

# ── Profile upgrade mode ──────────────────────────────────────────────────────

function Invoke-UpgradeProfile {
    param([string]$targetProfile)

    $validProfiles = @('Minimal', 'Fast', 'Balanced', 'Deep')

    # Normalize casing so "balanced" works as well as "Balanced"
    $matched = $validProfiles | Where-Object { $_ -ieq $targetProfile } | Select-Object -First 1
    if ($null -eq $matched) {
        Write-Fail "Unknown profile: '$targetProfile'."
        Write-Info "Valid profiles: $($validProfiles -join ', ')"
        exit 1
    }
    $targetProfile = $matched

    Write-Head "Upgrade to $targetProfile Profile"
    Write-Info "Target profile : $targetProfile  ($($PROFILE_DISK[$targetProfile]))"
    Write-Info "Required models: $($PROFILE_MODELS[$targetProfile] -join ', ')"

    # ── Pre-flight: disk ──────────────────────────────────────────────────────
    $drive = Get-PSDrive C -ErrorAction SilentlyContinue
    if ($drive) { $Script:FreeDisk = [math]::Round($drive.Free / 1GB, 1) }
    Write-Info "Free disk      : $($Script:FreeDisk) GB"

    # ── Pre-flight: internet (Issue 5) ────────────────────────────────────────
    # Check internet separately from Ollama API — they are independent.
    Write-Info 'Checking internet connectivity...'
    $hasNet = Test-InternetConnectivity
    if (-not $hasNet) {
        Write-Warn 'Internet not reachable (8.8.8.8 timeout).'
        # If all required models are already installed we can proceed without downloading.
        $alreadyMissing = Get-MissingRequiredModels $targetProfile
        if ($alreadyMissing.Count -gt 0) {
            Write-Fail "No internet AND required models are missing — cannot proceed."
            Write-Fail "Missing: $($alreadyMissing -join ', ')"
            Write-Info 'Connect to the internet or pull models manually:'
            foreach ($m in $alreadyMissing) { Write-Info "  ollama pull $m" }
            exit 1
        }
        Write-Ok 'All required models already installed — skipping download.'
    } else {
        Write-Ok 'Internet reachable.'
    }

    # ── Pre-flight: Ollama API ────────────────────────────────────────────────
    $ollamaOk = Start-OllamaServe
    if (-not $ollamaOk) {
        Write-Fail 'Ollama API not reachable — cannot verify or pull models. Start Ollama and retry.'
        exit 1
    }

    # ── Pre-flight: disk space (only missing models counted) ──────────────────
    $diskOk = Test-DiskForProfile $targetProfile
    if (-not $diskOk) {
        Write-Fail "Insufficient disk space for $targetProfile profile."
        Write-Info 'Free up disk space and retry, or choose a lighter profile:'
        Write-Info '  .\setup.ps1 -UpgradeProfile Fast'
        Write-Info '  .\setup.ps1 -UpgradeProfile Minimal'
        exit 1
    }

    # ── Pull missing required + optional models ───────────────────────────────
    Install-Models $targetProfile

    # ── Integrity check: verify every required model actually installed (Issue 1) ──
    Write-Head "Verifying $targetProfile Models"
    $missing = Get-MissingRequiredModels $targetProfile
    if ($missing.Count -gt 0) {
        Write-Host ''
        Write-Fail "$targetProfile upgrade INCOMPLETE - required models not installed:"
        foreach ($m in $missing) { Write-Host "  [MISSING] $m" -ForegroundColor Red }
        Write-Host ''
        Write-Info 'models.json was NOT updated. Active profile is unchanged.'
        Write-Info 'Fix and retry:'
        Write-Info "  .\setup.ps1 -UpgradeProfile $targetProfile"
        Write-Info 'Or pull manually and then re-run:'
        foreach ($m in $missing) { Write-Info "  ollama pull $m" }
        exit 1
    }
    Write-Ok "All required $targetProfile models verified installed."

    # ── Write new model routing (only reached if all models confirmed) ────────
    Write-Head 'Updating Model Routing'
    Write-ModelsJson $targetProfile

    # ── Validate with mera -Doctor and mera -Health ───────────────────────────
    Write-Head 'Validation'
    if (Test-Path $MERA_EXE_DST) {
        Push-Location $MERA_INSTALL
        try {
            Write-Info 'mera -Doctor'
            & $MERA_EXE_DST '-Doctor' 2>&1 | ForEach-Object { Write-Host "  $_"; Append-Log "  [upgrade-validate] $_" }

            Write-Info 'mera -Health'
            $healthOut = & $MERA_EXE_DST '-Health' 2>&1
            $healthOut | ForEach-Object { Write-Host "  $_"; Append-Log "  [upgrade-validate] $_" }
            $scoreLine = $healthOut | Where-Object { "$_" -match 'HEALTH:\s*(\d+)%' } | Select-Object -First 1
            if ($scoreLine -match 'HEALTH:\s*(\d+)%') {
                $score = [int]$Matches[1]
                if ($score -ge 70) {
                    Write-Ok "Health: $score% — installation healthy"
                } else {
                    Write-Warn "Health: $score% — run 'mera -Doctor' to investigate"
                }
            }
        } finally {
            Pop-Location
        }
    } else {
        Write-Warn "mera.exe not found at $MERA_EXE_DST — skipping validation."
        Write-Info "Run 'mera -Doctor' manually after setup."
    }

    Write-Sep
    Write-Ok "Profile upgraded to $targetProfile."
    Write-Ok "models.json updated — new role routing is active."
    Write-Info "Run 'mera -Models' to verify model assignments."
    if ($targetProfile -eq 'Minimal') { Show-MinimalWarning }
    Write-Sep
}

# ── Main ──────────────────────────────────────────────────────────────────────

function Main {
    try { $host.UI.RawUI.WindowTitle = "MERA $MERA_VERSION Installer" } catch {}

    Write-Host ''
    Write-Host '+======================================================+' -ForegroundColor Cyan
    Write-Host "|  MERA AI Engineering OS  $MERA_VERSION                      |" -ForegroundColor Cyan
    Write-Host '|  Enterprise Installer for Windows                    |' -ForegroundColor Cyan
    Write-Host '+======================================================+' -ForegroundColor Cyan
    Write-Host ''
    Show-ReleaseInfo

    # Ensure install dir exists for logging from the start
    New-Item -ItemType Directory -Path $MERA_INSTALL -Force -ErrorAction SilentlyContinue | Out-Null

    if ($Update)                { Invoke-Update;                      return }
    if ($Uninstall)             { Invoke-Uninstall;                   return }
    if ($Repair)                { Invoke-Repair;                      return }
    if ($UpgradeProfile -ne '') { Invoke-UpgradeProfile $UpgradeProfile; return }

    # ── Checkpoint: resume detection ───────────────────────────────────────

    $cp = Get-Checkpoint
    $resuming = $false
    if ($cp.startedAt -ne '' -and -not $cp.complete) {
        Write-Warn "A previous install started at $($cp.startedAt) was not completed."
        if (Prompt-YN 'Resume from checkpoint?') {
            $resuming = $true
            Write-Info "Resuming install (profile: $($cp.profile))"
        } else {
            Clear-Checkpoint
            $cp = Get-Checkpoint
            Write-Info 'Starting fresh install.'
        }
    }

    # ── Install flow ───────────────────────────────────────────────────────

    if (-not $resuming) {
        Invoke-SysInfo
        Invoke-ToolDetect
    }

    $selectedProfile = if ($resuming -and $cp.profile -ne '') { $cp.profile } else { Select-Profile }
    Write-Info "Selected profile: $selectedProfile  ($($PROFILE_DISK[$selectedProfile]))"
    Append-Log "Profile: $selectedProfile"

    if (-not $resuming) {
        $cp.startedAt = (Get-Date -Format 'yyyy-MM-dd HH:mm:ss')
        $cp.profile   = $selectedProfile
        Save-Checkpoint $cp
    }

    # Tools
    if (-not $cp.python) {
        Install-Python
        $cp.python = $true; Save-Checkpoint $cp
    } else { Write-Ok 'Python: skipped (checkpoint OK)' }

    $ollamaOk = $false
    if (-not $cp.ollama) {
        Install-Ollama
        $ollamaOk = Start-OllamaServe
        $cp.ollama = $true; Save-Checkpoint $cp
    } else {
        $ollamaOk = Start-OllamaServe
        Write-Ok 'Ollama: skipped install (checkpoint OK)'
    }

    if (-not $cp.aider) {
        Install-Aider
        $cp.aider = $true; Save-Checkpoint $cp
    } else { Write-Ok 'Aider: skipped (checkpoint OK)' }

    # Filesystem + binary
    if (-not $cp.directories) {
        New-MeraDirectories
        $cp.directories = $true; Save-Checkpoint $cp
    } else { Write-Ok 'Directories: skipped (checkpoint OK)' }

    if (-not $cp.binary) {
        Copy-MeraExe
        $cp.binary = $true; Save-Checkpoint $cp
    } else { Write-Ok 'Binary: skipped (checkpoint OK)' }

    # Configs
    if (-not $cp.configFiles) {
        Write-Head 'Config Generation'
        Write-ModelsJson $selectedProfile
        Write-ConfigJson
        Write-AiderFiles
        $cp.configFiles = $true; Save-Checkpoint $cp
    } else { Write-Ok 'Config files: skipped (checkpoint OK)' }

    # Models
    if (-not $cp.models) {
        if ($ollamaOk) {
            $diskOk = Test-DiskForProfile $selectedProfile
            if ($diskOk) {
                Install-Models $selectedProfile
            } else {
                Write-Warn 'Model install skipped due to insufficient disk space.'
                Write-Warn "Free up space and re-run: .\setup.ps1 -$selectedProfile -Silent"
                $Script:Failed.Add("Models skipped — insufficient disk for $selectedProfile") | Out-Null
            }
        } else {
            Write-Warn 'Ollama not running — skipping model pull. Pull manually: ollama pull <model>'
        }
        $cp.models = $true; Save-Checkpoint $cp
    } else { Write-Ok 'Models: skipped (checkpoint OK)' }

    # PATH
    if (-not $cp.path) {
        Invoke-PathSetup
        $cp.path = $true; Save-Checkpoint $cp
    } else { Write-Ok 'PATH: skipped (checkpoint OK)' }

    # Verify mera is globally accessible
    Test-MeraGlobal

    # Mark complete and remove checkpoint
    $cp.complete = $true
    Save-Checkpoint $cp
    Clear-Checkpoint

    # Validate
    Invoke-Validation

    # Report
    Write-Report $selectedProfile

    # Minimal profile advisory — shown after the report so it's the last thing the user reads
    if ($selectedProfile -eq 'Minimal') { Show-MinimalWarning }

    # Final banner
    Write-Sep
    if ($Script:Failed.Count -eq 0) {
        Write-Host "  MERA $MERA_VERSION installation complete!" -ForegroundColor Green
    } else {
        Write-Host "  MERA $MERA_VERSION installed with $($Script:Failed.Count) issue(s)." -ForegroundColor Yellow
        Write-Host "  See: $REPORT_FILE" -ForegroundColor Yellow
    }
    Write-Sep
    Write-Host ''
    Write-Host '  Get started:' -ForegroundColor White
    Write-Host "    cd $MERA_INSTALL"
    Write-Host '    mera -Doctor'
    Write-Host '    mera -Init           # run in your project repo'
    Write-Host '    mera -DryRun <module> "your task"'
    Write-Host ''
    Write-Host '  Open a NEW terminal to ensure PATH changes take effect.' -ForegroundColor Yellow
    Write-Host ''
}

Main
