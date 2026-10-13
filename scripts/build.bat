@echo off
REM Ai-Shell build script (Windows, cmd)
REM Usage:
REM   scripts\build.bat          build only
REM   scripts\build.bat run      build and launch
REM   scripts\build.bat dev      dev mode (hot reload)
setlocal EnableDelayedExpansion

set "ROOT_DIR=%~dp0.."
pushd "%ROOT_DIR%" || (echo [ERROR] Cannot enter project root & exit /b 1)

set "APP_NAME=Ai-Shell"
set "ACTION=%~1"
if "%ACTION%"=="" set "ACTION=build"

where go >nul 2>nul || (echo [ERROR] Go is not installed. Get it from https://go.dev/dl/ & popd & exit /b 1)
where npm >nul 2>nul || (echo [ERROR] npm is not installed. Get Node.js from https://nodejs.org/ & popd & exit /b 1)

REM Ensure wails (installed via "go install") is on PATH for this session
for /f "delims=" %%i in ('go env GOPATH') do set "GOPATH=%%i"
set "PATH=%PATH%;%GOPATH%\bin"

where wails >nul 2>nul || (
    echo [INFO] Installing wails CLI...
    call go install github.com/wailsapp/wails/v2/cmd/wails@latest
    if errorlevel 1 (echo [ERROR] Failed to install wails CLI & popd & exit /b 1)
)

REM Frontend deps
if not exist "frontend\node_modules" (
    echo [INFO] Installing frontend dependencies...
    pushd frontend
    call npm install
    if errorlevel 1 (echo [ERROR] npm install failed & popd & popd & exit /b 1)
    popd
)

if /i "%ACTION%"=="dev" (
    call wails dev
    set "RC=!errorlevel!"
    popd
    exit /b !RC!
)

echo [INFO] Building for Windows...
REM -webview2 embed: bundle WebView2 bootstrapper so machines without WebView2 can still run
call wails build -clean -trimpath -webview2 embed -platform windows/amd64
if errorlevel 1 (echo [ERROR] Build failed & popd & exit /b 1)

set "EXE=%ROOT_DIR%\build\bin\%APP_NAME%.exe"
if not exist "%EXE%" (
    echo [ERROR] Build output not found: %EXE%
    popd
    exit /b 1
)

echo [OK] Build finished: %ROOT_DIR%\build\bin\

if /i "%ACTION%"=="run" (
    echo [INFO] Launching %APP_NAME%...
    start "" "%EXE%"
)

popd
endlocal
