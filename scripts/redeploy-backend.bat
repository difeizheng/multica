@echo off
REM Wrapper that runs redeploy-backend.sh via Git Bash, so it can be invoked
REM from cmd / PowerShell / double-click on Windows.
REM
REM Usage (from anywhere):
REM   scripts\redeploy-backend.bat                 rebuild all + redeploy
REM   scripts\redeploy-backend.bat server          rebuild only the server bin
REM   scripts\redeploy-backend.bat --skip-build    reuse existing binaries
setlocal

set "SCRIPT=%~dp0redeploy-backend.sh"

REM 1) Try bash on PATH (Git for Windows usually adds it).
where bash >nul 2>&1 && (
  bash "%SCRIPT%" %*
  goto :done
)

REM 2) Try common Git Bash install locations.
for %%G in (
  "%ProgramFiles%\Git\bin\bash.exe"
  "%ProgramFiles(x86)%\Git\bin\bash.exe"
  "%ProgramFiles%\Git\usr\bin\bash.exe"
  "D:\software\Git\bin\bash.exe"
  "D:\software\PortableGit\bin\bash.exe"
) do (
  if exist "%%~G" (
    "%%~G" "%SCRIPT%" %*
    goto :done
  )
)

echo ERROR: Git Bash (bash.exe) not found. >&2
echo Install Git for Windows, or run redeploy-backend.sh inside Git Bash/WSL. >&2
exit /b 1

:done
endlocal & exit /b %ERRORLEVEL%
