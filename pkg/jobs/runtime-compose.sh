#!/bin/sh
# Canonical apt host preparation. Run as root; safe to repeat on an existing host.
# Embedded by runtime_bootstrap.go; sync-runtime-compose.py projects the same
# bytes into the single-file installer and guest lifecycle entry points.
# Callers may reuse their apt update and retain their IPv4 policy. A newly added
# repository still needs its own index refresh.
set -eu
key=
runtime_exit() {
    code=$?
    trap - EXIT
    if [ -n "$key" ]; then rm -f "$key"; fi
    if [ "$code" -ne 0 ]; then
        echo 'Docker Engine or Compose preparation failed; runtime is not ready. See https://docs.docker.com/engine/install/.' >&2
    fi
    exit "$code"
}
trap runtime_exit EXIT
trap 'exit 1' HUP INT TERM

apt_runtime() {
    if [ "${KOMBIFY_RUNTIME_APT_FORCE_IPV4:-0}" = 1 ]; then
        set -- -o Acquire::ForceIPv4=true "$@"
    fi
    timeout 240 env DEBIAN_FRONTEND=noninteractive apt-get \
        -o DPkg::Lock::Timeout=120 -o Acquire::Retries=2 \
        -o Acquire::http::Timeout=25 -o Acquire::https::Timeout=25 \
        --no-remove "$@"
}
compose_ready() {
    docker compose version --short >/dev/null 2>&1
}
candidate_version() {
    apt-cache policy "$1" | awk '/Candidate:/ && $2 != "(none)" {print $2; exit}'
}

if command -v docker >/dev/null 2>&1 && compose_ready; then
    echo 'compose_prerequisite=already_ready'
    exit 0
fi
if [ "${KOMBIFY_RUNTIME_APT_UPDATED:-0}" != 1 ]; then apt_runtime update; fi
apt_runtime install -y ca-certificates curl
# Choose Compose before installing an Engine. Debian 12's Python v1 package
# cannot supply the CLI plugin; a fresh host needs the Docker repository pair.
compose_package=
for package in docker-compose-v2 docker-compose; do
    version=$(candidate_version "$package")
    [ -n "$version" ] || continue
    # Debian uses the same name for Python Compose v1 and the CLI plugin v2.
    if [ "$package" = docker-compose ] && ! dpkg --compare-versions "$version" ge 2; then
        continue
    fi
    compose_package=$package
    break
done
if [ -n "$compose_package" ]; then
    echo "compose_package=$compose_package compose_source=configured_apt"
    # A distro plugin can depend on docker.io. Refuse to replace an existing
    # Engine; if that dependency conflicts, install Docker's independent plugin.
    if command -v docker >/dev/null 2>&1; then
        if apt_runtime install -y "$compose_package" && compose_ready; then exit 0; fi
    else
        if apt_runtime install -y docker.io "$compose_package" && compose_ready; then exit 0; fi
    fi
fi

# Existing CLI/Engine: request only the plugin. Fresh host without distribution
# Compose v2: install both Engine and plugin from Docker's supported repository.
engine_packages=
if ! command -v docker >/dev/null 2>&1; then
    engine_packages='docker-ce docker-ce-cli containerd.io'
fi
# shellcheck disable=SC1091
. /etc/os-release
case "$ID" in
    debian|ubuntu) ;;
    *) echo "Docker Compose is missing: no Docker apt repository configured for $ID." >&2; exit 1 ;;
esac
: "${VERSION_CODENAME:?Docker Compose is missing: /etc/os-release needs VERSION_CODENAME}"
install -d -m 0755 /etc/apt/keyrings
key=$(mktemp)
curl --connect-timeout 10 --max-time 120 -fsSL "https://download.docker.com/linux/$ID/gpg" -o "$key"
install -m 0644 "$key" /etc/apt/keyrings/kombify-docker.asc
printf 'deb [arch=%s signed-by=/etc/apt/keyrings/kombify-docker.asc] https://download.docker.com/linux/%s %s stable\n' \
    "$(dpkg --print-architecture)" "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/kombify-docker-compose.list
# Admit Engine candidates only for a fresh Docker install. On an existing
# distribution Engine, the repository may supply only the missing plugin.
cat > /etc/apt/preferences.d/kombify-docker-compose <<PREFERENCES
Package: docker-compose-plugin $engine_packages
Pin: origin download.docker.com
Pin-Priority: 500

Package: *
Pin: origin download.docker.com
Pin-Priority: -1
PREFERENCES
apt_runtime update
echo "compose_package=docker-compose-plugin compose_source=docker_repository engine_packages=${engine_packages:-unchanged}"
# Intentional word splitting of this fixed package list; never add an Engine
# package when the host already has its Docker CLI.
# shellcheck disable=SC2086
apt_runtime install -y $engine_packages docker-compose-plugin
if ! compose_ready; then
    echo 'Docker Compose is still missing after package installation; runtime is not ready. See https://docs.docker.com/compose/install/linux/.' >&2
    exit 1
fi
docker compose version --short
