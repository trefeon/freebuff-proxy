@echo off
rem install.bat - wrapper for install-freebuff-proxy.ps1 with ExecutionPolicy Bypass
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-freebuff-proxy.ps1" %*
