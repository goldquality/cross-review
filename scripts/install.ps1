# Установка и обновление клиента cross-review на Windows. В PowerShell:
#
#   [Net.ServicePointManager]::SecurityProtocol='Tls12'; iex ([Text.Encoding]::UTF8.GetString((iwr -useb https://github.com/goldquality/cross-review/releases/latest/download/install.ps1).RawContentStream.ToArray()))
#
# Не irm | iex: файлы релиза GitHub отдаёт без кодировки, и Windows PowerShell 5.1 прочёл бы русский
# текст скрипта не как UTF-8.
#
# Скачивает последний релиз, сверяет его с SHA256SUMS, кладёт в %LOCALAPPDATA%\Programs\cross-review,
# включает автозапуск и перезапускает клиент. Повторный запуск — обновление. Файл настроек
# cross-review.yaml в домашней папке скрипт не трогает: его кладёт сам человек.
#
# $env:CROSS_REVIEW_VERSION = 'v0.1.4' — поставить эту версию, а не последнюю.
#
# Всё внутри блока & { }: iex выполняет скрипт в сессии человека, а настройки и переменные блока
# не должны в ней остаться. По той же причине нет exit: он закрыл бы окно PowerShell.
& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # с полосой прогресса Invoke-WebRequest в 5.1 качает в разы медленнее
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'goldquality/cross-review'
    $base = if ($env:CROSS_REVIEW_VERSION) {
        "https://github.com/$repo/releases/download/$env:CROSS_REVIEW_VERSION"
    } else {
        "https://github.com/$repo/releases/latest/download"
    }
    # PROCESSOR_ARCHITEW6432 задан, если PowerShell 32-битный на 64-битной Windows
    $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($cpu) { 'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { $null } }
    if (-not $arch) {
        Write-Host "ошибка: неподдерживаемая архитектура $cpu" -ForegroundColor Red
        return
    }
    $asset = "cross-review-windows-$arch.zip"
    $dir = Join-Path $env:LOCALAPPDATA 'Programs\cross-review'
    $exe = Join-Path $dir 'cross-review.exe'
    $settingsDir = if ($env:CROSS_REVIEW_HOME) { $env:CROSS_REVIEW_HOME } else { $HOME }
    $settings = Join-Path $settingsDir 'cross-review.yaml'
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('cross-review-' + [guid]::NewGuid())
    New-Item -ItemType Directory -Force $tmp | Out-Null
    try {
        Write-Host "Скачиваю ${asset}…"
        $zip = Join-Path $tmp $asset
        $sums = Join-Path $tmp 'SHA256SUMS'
        Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile $zip
        Invoke-WebRequest -UseBasicParsing "$base/SHA256SUMS" -OutFile $sums
        $want = Get-Content $sums | ForEach-Object {
            $f = -split $_
            if ($f.Count -ge 2 -and ($f[1] -eq $asset -or $f[1] -eq "*$asset")) { $f[0] }
        } | Select-Object -First 1
        $got = (Get-FileHash -Algorithm SHA256 $zip).Hash
        if (-not $want -or $got -ne $want) { throw "контрольная сумма $asset не совпала с SHA256SUMS" }
        Expand-Archive -Force $zip (Join-Path $tmp 'x')

        # второй экземпляр трея только откроет окно первого — старый нужно остановить
        Get-Process cross-review -ErrorAction SilentlyContinue | Stop-Process -Force
        Get-Process cross-review -ErrorAction SilentlyContinue | Wait-Process -Timeout 10 -ErrorAction SilentlyContinue
        New-Item -ItemType Directory -Force $dir | Out-Null
        Copy-Item -Force (Join-Path $tmp 'x\cross-review.exe') $exe
        Unblock-File $exe # пометка «скачано из интернета», из-за неё SmartScreen спрашивает при запуске

        # бинарь собран как GUI-приложение: без -Wait PowerShell не дождался бы его и не узнал код выхода
        $p = Start-Process -FilePath $exe -ArgumentList 'install' -NoNewWindow -Wait -PassThru
        if ($p.ExitCode -ne 0) { throw "cross-review install завершился с кодом $($p.ExitCode)" }
        Write-Host "cross-review установлен: $exe"

        # окна клиента — WebView2: он есть в Windows 10 и 11, но его могли удалить
        $wv2 = '{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}'
        $hasWebView2 = @("HKLM:\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\$wv2",
            "HKLM:\SOFTWARE\Microsoft\EdgeUpdate\Clients\$wv2",
            "HKCU:\SOFTWARE\Microsoft\EdgeUpdate\Clients\$wv2") | Where-Object { Test-Path $_ }
        if (-not $hasWebView2) {
            Write-Host 'Для окон клиента нужен WebView2: https://developer.microsoft.com/microsoft-edge/webview2/' -ForegroundColor Yellow
        }

        if (Test-Path $settings) {
            Start-Process -FilePath $exe
            Write-Host 'Клиент запущен: иконка в трее у часов, может прятаться под стрелкой ^.'
        } else {
            Write-Host ''
            Write-Host "Файла настроек нет: $settings"
            Write-Host 'Положи туда cross-review.yaml от администратора брокера и запусти эту команду ещё раз.'
        }
    } catch {
        Write-Host "ошибка: $_" -ForegroundColor Red
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}
