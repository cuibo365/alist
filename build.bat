@echo off
setlocal
rem ============================================================
rem  alist custom build script (minimal size, no debug info)
rem  Usage: double-click, or run in CMD:  build.bat
rem ============================================================

rem ---- toolchain paths (edit here if they move) ----
set "GO_BIN=D:\tools\go\bin"
set "MINGW_BIN=D:\tools\mingw64\bin"
set "GOPATH_DIR=D:\tools\gopath"

rem ---- version stamp shown by `alist version` ----
set "APP_VERSION=v3.66.0-c1"

rem ---- environment ----
set "PATH=%GO_BIN%;%MINGW_BIN%;%PATH%"
set "CGO_ENABLED=1"
set "GOPATH=%GOPATH_DIR%"
set "GOMODCACHE=%GOPATH_DIR%\pkg\mod"
set "GOPROXY=https://goproxy.cn,direct"

rem ---- go to repo root (this bat lives there) ----
cd /d "%~dp0"
if errorlevel 1 goto :fail

go version || goto :fail

echo.
echo Building alist (%APP_VERSION%) ...
go build -trimpath -ldflags "-s -w -X github.com/alist-org/alist/v3/internal/conf.Version=%APP_VERSION%" -o alist-c.exe .
if errorlevel 1 goto :fail

echo.
echo ==============================================
echo  BUILD OK: %~dp0alist-c.exe
for %%F in (alist-c.exe) do echo  SIZE: %%~zF bytes
echo ==============================================
exit /b 0

:fail
echo.
echo BUILD FAILED - see error messages above.
exit /b 1
