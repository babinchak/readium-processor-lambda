# PowerShell build script for Lambda function

param(
    [Parameter(Mandatory=$false)]
    [ValidateSet("build", "clean", "deps", "all")]
    [string]$Action = "build"
)

$ErrorActionPreference = "Stop"

function Build-Lambda {
    Write-Host "Building Lambda function for ARM64..." -ForegroundColor Green
    
    $env:GOOS = "linux"
    $env:GOARCH = "arm64"
    $env:CGO_ENABLED = "0"
    
    go build -tags lambda.norpc -o bootstrap main.go
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Build failed!" -ForegroundColor Red
        exit 1
    }
    
    if (Test-Path "function.zip") {
        Remove-Item "function.zip"
    }
    
    Compress-Archive -Path bootstrap -DestinationPath function.zip -Force
    Write-Host "Created function.zip" -ForegroundColor Green
}

function Clean-Build {
    Write-Host "Cleaning build artifacts..." -ForegroundColor Yellow
    
    if (Test-Path "bootstrap") {
        Remove-Item "bootstrap"
    }
    if (Test-Path "function.zip") {
        Remove-Item "function.zip"
    }
    Write-Host "Clean complete" -ForegroundColor Green
}

function Update-Dependencies {
    Write-Host "Updating dependencies..." -ForegroundColor Green
    go mod download
    go mod tidy
    Write-Host "Dependencies updated" -ForegroundColor Green
}

switch ($Action) {
    "build" {
        Build-Lambda
    }
    "clean" {
        Clean-Build
    }
    "deps" {
        Update-Dependencies
    }
    "all" {
        Update-Dependencies
        Build-Lambda
    }
}

Write-Host "`nDone!" -ForegroundColor Green

