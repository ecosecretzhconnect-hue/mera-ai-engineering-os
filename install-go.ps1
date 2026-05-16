$ErrorActionPreference = "Stop"
$installDir = "C:\Tools\MERA"
New-Item -ItemType Directory -Force -Path $installDir | Out-Null

if (!(Test-Path ".\mera.exe")) {
    if (!(Get-Command go -ErrorAction SilentlyContinue)) {
        throw "Go is not installed. Install Go, then run build.ps1."
    }
    go build -o mera.exe .
}

Copy-Item ".\mera.exe" $installDir -Force
$currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($currentPath -notlike "*$installDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$installDir", "User")
}
Write-Host "[OK] MERA Go installed to C:\Tools\MERA\mera.exe" -ForegroundColor Green
Write-Host "Restart PowerShell and run: mera -Doctor"
