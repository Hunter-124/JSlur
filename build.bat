@echo off
REM ============================================================
REM  AutoApply - build script (Windows)
REM  Compiles the app into autoapply.exe with the web UI embedded.
REM ============================================================
setlocal

cd /d "%~dp0"

echo.
echo  Building AutoApply...
echo.

REM This is a Wails app: it MUST be built with the Wails CLI so the correct
REM build tags (desktop,production) and the WebView2 loader are injected. A
REM plain "go build" produces a binary that errors at launch about missing
REM Wails tags, so we deliberately do NOT use it here.
REM
REM Output lands in build\bin\autoapply.exe -- run THAT one.
wails build -platform windows/amd64 -trimpath
if errorlevel 1 (
    echo.
    echo  BUILD FAILED.
    echo.
    pause
    exit /b 1
)

echo.
echo  Build succeeded -^> build\bin\autoapply.exe
echo.
pause
endlocal
