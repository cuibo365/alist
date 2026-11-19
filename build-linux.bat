@echo off
setlocal
rem ============================================================
rem  alist custom build script - Linux (static musl, CGO)
rem  Usage: double-click, or run in CMD:  build-linux.bat
rem  Output: alist-c-linux-amd64   (ELF x86_64, statically linked)
rem ============================================================

rem ---- toolchain paths (edit here if they move) ----
set "GO_BIN=D:\tools\go\bin"
set "ZIG_BIN=D:\tools\zig"
set "GOPATH_DIR=D:\tools\gopath"

rem ---- version stamp shown by `alist version` ----
set "APP_VERSION=v3.66.0-c1"

rem ---- environment ----
set "PATH=%GO_BIN%;%ZIG_BIN%;%PATH%"
rem go-sqlite3 is C code, so CGO is required. zig cc supplies the Linux
rem headers and musl libc that mingw cannot, and links the result statically
rem so the binary carries no glibc version requirement.
set "CGO_ENABLED=1"
set "GOOS=linux"
set "GOARCH=amd64"
set "CC=%ZIG_BIN%\zig.exe cc -target x86_64-linux-musl"
set "GOPATH=%GOPATH_DIR%"
set "GOMODCACHE=%GOPATH_DIR%\pkg\mod"
set "GOPROXY=https://goproxy.cn,direct"

rem ---- go to repo root (this bat lives there) ----
cd /d "%~dp0"
if errorlevel 1 goto :fail

go version || goto :fail
zig version || goto :fail

echo.
echo Building alist for linux/amd64 (%APP_VERSION%) ...
go build -trimpath -ldflags "-s -w -X github.com/alist-org/alist/v3/internal/conf.Version=%APP_VERSION%" -o alist-c-linux-amd64 .
if errorlevel 1 goto :fail

echo.
echo ==============================================
echo  BUILD OK: %~dp0alist-c-linux-amd64
for %%F in (alist-c-linux-amd64) do echo  SIZE: %%~zF bytes
echo ==============================================
exit /b 0

:fail
echo.
echo BUILD FAILED - see error messages above.
exit /b 1
