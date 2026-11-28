#!/bin/sh
# Установка и обновление клиента cross-review на macOS и Linux:
#
#   curl -fsSL https://github.com/goldquality/cross-review/releases/latest/download/install.sh | sh
#
# Скачивает последний релиз, сверяет его с SHA256SUMS, кладёт в ~/.local/bin, включает автозапуск и
# перезапускает клиент. Повторный запуск — обновление. Файл настроек cross-review.yaml скрипт не
# трогает: его кладёт сам человек.
#
# CROSS_REVIEW_VERSION=v0.1.4 — поставить эту версию, а не последнюю.
#
# Всё внутри main: если curl оборвётся на середине, sh не выполнит половину скрипта.
set -eu

REPO=goldquality/cross-review
LABEL=com.github.goldquality.cross-review

say() { printf '%s\n' "$*"; }
die() {
	printf 'ошибка: %s\n' "$*" >&2
	exit 1
}

# cleanup — убрать временную папку, сохранив код выхода: без явного exit bash 3.2 на macOS после
# ошибки в main выходил с 0.
cleanup() {
	rc=$?
	rm -rf "$tmp"
	exit "$rc"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	else
		shasum -a 256 "$1" | cut -d ' ' -f 1
	fi
}

# stop — остановить запущенный клиент: второй экземпляр трея только скажет «уже запущен».
stop() {
	uid=$(id -u)
	pkill -x -u "$uid" cross-review 2>/dev/null || return 0
	i=0
	while pgrep -x -u "$uid" cross-review >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 50 ]; then
			pkill -9 -x -u "$uid" cross-review 2>/dev/null || true
			break
		fi
		sleep 0.1
	done
}

# start_macos — перечитать LaunchAgent и запустить трей. bootout, а не kickstart: install мог
# переписать путь к бинарю, а kickstart запустил бы старый.
start_macos() {
	domain=gui/$(id -u)
	launchctl bootout "$domain/$LABEL" 2>/dev/null || true
	for _ in 1 2 3; do
		launchctl bootstrap "$domain" "$HOME/Library/LaunchAgents/$LABEL.plist" 2>/dev/null && return 0
		sleep 1
	done
	die "launchctl не запустил клиент: запусти $1 вручную"
}

start_linux() {
	if [ -z "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
		say "Графической сессии нет: трей запустится при следующем входе. Без трея: $1 daemon"
		return 0
	fi
	# окна клиента — GTK 4 и WebKitGTK 6.0, они грузятся при запуске: без них клиент не стартует
	libs=$( (ldconfig -p || /sbin/ldconfig -p) 2>/dev/null || true)
	missing=
	if [ -n "$libs" ]; then
		case $libs in *libgtk-4.so.1*) ;; *) missing="$missing libgtk-4-1" ;; esac
		case $libs in *libwebkitgtk-6.0.so.4*) ;; *) missing="$missing libwebkitgtk-6.0-4" ;; esac
	fi
	if [ -n "$missing" ]; then
		say "Для окон не хватает:$missing. Например: sudo apt install$missing (Fedora: sudo dnf install gtk4 webkitgtk6.0)"
		say "Поставь их и запусти $1. Без окон: $1 daemon"
		return 0
	fi
	setsid -f "$1" >/dev/null 2>&1 </dev/null || nohup "$1" >/dev/null 2>&1 </dev/null &
	say "Клиент запущен: иконка в трее."
}

main() {
	case "$(uname -s)" in
	Darwin) os=macos ;;
	Linux) os=linux ;;
	*) die "этот скрипт для macOS и Linux, на Windows — install.ps1" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "неподдерживаемая архитектура $(uname -m)" ;;
	esac
	if [ "$os" = macos ]; then
		asset=cross-review-macos.zip
	else
		asset=cross-review-linux-$arch.tar.gz
	fi
	if [ -n "${CROSS_REVIEW_VERSION:-}" ]; then
		base=https://github.com/$REPO/releases/download/$CROSS_REVIEW_VERSION
	else
		base=https://github.com/$REPO/releases/latest/download
	fi
	command -v curl >/dev/null 2>&1 || die "нужен curl"

	tmp=$(mktemp -d)
	trap cleanup EXIT
	# фигурные скобки обязательны: без них bash 3.2 на macOS читает байты «…» как часть имени
	say "Скачиваю ${asset}…"
	curl -fsSL -o "$tmp/$asset" "$base/$asset" || die "не скачался $base/$asset"
	curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "не скачался $base/SHA256SUMS"
	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
	got=$(sha256 "$tmp/$asset")
	if [ -z "$want" ] || [ "$want" != "$got" ]; then
		die "контрольная сумма $asset не совпала с SHA256SUMS"
	fi
	mkdir "$tmp/x"
	if [ "$os" = macos ]; then
		unzip -q "$tmp/$asset" cross-review -d "$tmp/x"
	else
		tar -xzf "$tmp/$asset" -C "$tmp/x" cross-review
	fi

	bin=$HOME/.local/bin
	exe=$bin/cross-review
	mkdir -p "$bin"
	stop
	# через переименование: файл заменяется, даже если старый процесс ещё не вышел
	cp "$tmp/x/cross-review" "$bin/.cross-review.new"
	chmod 755 "$bin/.cross-review.new"
	mv -f "$bin/.cross-review.new" "$exe"
	"$exe" install
	say "cross-review $("$exe" version): $exe"

	settings=${CROSS_REVIEW_HOME:-$HOME}/cross-review.yaml
	if [ ! -f "$settings" ]; then
		say ""
		say "Файла настроек нет: $settings"
		say "Положи туда cross-review.yaml от администратора брокера и запусти эту команду ещё раз."
		return 0
	fi
	if [ "$os" = macos ]; then
		start_macos "$exe"
		say "Клиент запущен: иконка в строке меню."
	else
		start_linux "$exe"
	fi
	case ":$PATH:" in
	*":$bin:"*) ;;
	*) say "Чтобы вызывать cross-review из терминала, добавь $bin в PATH." ;;
	esac
}

main "$@"
