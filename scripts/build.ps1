# Ai-Shell build script (Windows, PowerShell)
# Usage:
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1          # build only
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -Run     # build and launch
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -Dev     # dev mode (hot reload)
param(
    [switch]$Run,
    [switch]$Dev
)

$ErrorActionPreference = "Stop"
$RootDir = Split-Path -Parent $PSScriptRoot
Set-Location $RootDir

$AppName = "Ai-Shell"

function Check-Command($name, $installHint) {
    if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
        Write-Host "[ERROR] $name is not installed. $installHint" -ForegroundColor Red
        exit 1
    }
}

Check-Command "go"   "Install from https://go.dev/dl/"
Check-Command "npm"  "Install Node.js from https://nodejs.org/"

# Ensure wails (installed via `go install`) is on PATH for this session
$env:Path += ";$(go env GOPATH)\bin"

if (-not (Get-Command wails -ErrorAction SilentlyContinue)) {
    Write-Host "[INFO] Installing wails CLI..."
    go install github.com/wailsapp/wails/v2/cmd/wails@latest
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[ERROR] Failed to install wails CLI" -ForegroundColor Red
        exit 1
    }
}

# Frontend deps
if (-not (Test-Path "frontend\node_modules")) {
    Write-Host "[INFO] Installing frontend dependencies..."
    Push-Location frontend
    npm install
    $NpmExit = $LASTEXITCODE
    Pop-Location
    if ($NpmExit -ne 0) {
        Write-Host "[ERROR] npm install failed" -ForegroundColor Red
        exit 1
    }
}

if ($Dev) {
    wails dev
    exit $LASTEXITCODE
}

Write-Host "[INFO] Building for Windows..."
# -webview2 embed: bundle WebView2 bootstrapper so target machines without WebView2 can still run
wails build -clean -trimpath -webview2 embed
if ($LASTEXITCODE -ne 0) { exit 1 }

$Exe = Join-Path $RootDir "build\bin\$AppName.exe"
if (-not (Test-Path $Exe)) {
    Write-Host "[ERROR] Build output not found: $Exe" -ForegroundColor Red
    exit 1
}

Write-Host "[OK] Build finished: $(Join-Path $RootDir 'build\bin')" -ForegroundColor Green

if ($Run) {
    Write-Host "[INFO] Launching $AppName..."
    Start-Process $Exe
}
