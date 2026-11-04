#!/usr/bin/env bash

set -euo pipefail

# usage prints the setup helper's accepted arguments to standard error.
usage() {
    printf 'Usage: %s --setup [--no-env-file] [env-file] [github|gitlab|both]\n' "$(basename "$0")" >&2
}

# trim removes leading and trailing shell whitespace from its argument.
trim() {
    local value=$1
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "$value"
}

# env_value prints the requested key from ENV_FILE and returns failure when the
# file is not in use or does not define the key.
env_value() {
    local requested_key=$1
    local line key value result found=1

    if [[ -z "$ENV_FILE" ]]; then
        return 1
    fi

    while IFS= read -r line || [[ -n "$line" ]]; do
        line=$(trim "$line")
        [[ -z "$line" || "${line:0:1}" == '#' ]] && continue

        if [[ "$line" == export[[:space:]]* ]]; then
            line=$(trim "${line#export}")
        fi
        [[ "$line" == *=* ]] || continue

        key=$(trim "${line%%=*}")
        [[ "$key" == "$requested_key" ]] || continue

        value=$(trim "${line#*=}")
        if [[ ${#value} -ge 2 && "${value:0:1}" == "'" && "${value: -1}" == "'" ]]; then
            value=${value:1:${#value}-2}
        elif [[ ${#value} -ge 2 && "${value:0:1}" == '"' && "${value: -1}" == '"' ]]; then
            value=${value:1:${#value}-2}
        fi
        result=$value
        found=0
    done <"$ENV_FILE"

    if (( found == 0 )); then
        printf '%s' "$result"
    fi
    return "$found"
}

# resolve_path leaves absolute paths unchanged and anchors relative paths to base_dir.
resolve_path() {
    local path=$1
    local base_dir=$2

    if [[ "$path" == /* ]]; then
        printf '%s' "$path"
    else
        printf '%s/%s' "$base_dir" "$path"
    fi
}

# resolve_kubeconfig_list validates every entry of a colon-separated KUBECONFIG
# value and prints the resolved list. Relative entries are anchored to base_dir.
resolve_kubeconfig_list() {
    local list=$1
    local base_dir=$2
    local entry resolved output=""
    local entries=()

    IFS=: read -r -a entries <<<"$list"
    for entry in "${entries[@]}"; do
        [[ -z "$entry" ]] && continue
        resolved=$(resolve_path "$entry" "$base_dir")
        if [[ ! -r "$resolved" ]]; then
            printf 'KUBECONFIG entry is not readable: %s\n' "$resolved" >&2
            return 1
        fi
        output=${output:+$output:}$resolved
    done
    printf '%s' "$output"
}

if [[ "${1:-}" == "--setup" ]]; then
    shift
else
    printf '%s\n' 'The setup helper must be called with --setup.' >&2
    usage
    exit 2
fi

ENV_FILE=""
env_dir=""
env_file_input=""
use_env_file=1

while [[ $# -gt 0 ]]; do
    case "$1" in
        --no-env-file)
            use_env_file=0
            ;;
        github|gitlab|both)
            # Provider names are validated here; the setup steps are provider independent.
            ;;
        '')
            ;;
        *)
            if [[ -n "$env_file_input" ]]; then
                usage
                exit 2
            fi
            env_file_input=$1
            ;;
    esac
    shift
done

if (( use_env_file == 0 )); then
    if [[ -n "$env_file_input" ]]; then
        printf 'The setup helper cannot combine an env file with --no-env-file: %s\n' "$env_file_input" >&2
        usage
        exit 2
    fi
    env_dir=$PWD
else
    env_file_input=${env_file_input:-.konflux-test.env}
    if [[ "$env_file_input" == /* ]]; then
        ENV_FILE=$env_file_input
    else
        ENV_FILE=$PWD/$env_file_input
    fi
    if [[ ! -r "$ENV_FILE" ]]; then
        printf 'Env file is not readable: %s\n' "$ENV_FILE" >&2
        exit 1
    fi
    env_dir=$(cd -- "$(dirname -- "$ENV_FILE")" && pwd)
    ENV_FILE=$env_dir/$(basename -- "$ENV_FILE")
fi

kubeconfig_value=${KUBECONFIG:-}
if [[ -z "$kubeconfig_value" ]] && (( use_env_file )); then
    kubeconfig_value=$(env_value KUBECONFIG || true)
fi
if [[ -z "$kubeconfig_value" ]]; then
    printf 'KUBECONFIG is required in the environment or in the env file: %s\n' "${ENV_FILE:-none}" >&2
    exit 1
fi

kubeconfig_list=$(resolve_kubeconfig_list "$kubeconfig_value" "$env_dir") || exit 1
if [[ -z "$kubeconfig_list" ]]; then
    printf 'KUBECONFIG does not name any kubeconfig file\n' >&2
    exit 1
fi
KUBECONFIG=$kubeconfig_list
export KUBECONFIG

oc_command=${KONFLUX_TEST_OC:-oc}
cluster_server=$("$oc_command" whoami --show-server) || {
    printf 'Unable to determine cluster server with %s whoami --show-server\n' "$oc_command" >&2
    exit 1
}
if [[ -z "$cluster_server" ]]; then
    printf 'oc returned an empty cluster server\n' >&2
    exit 1
fi

expected_server=${KONFLUX_CLUSTER_SERVER:-}
if [[ -z "$expected_server" ]] && (( use_env_file )); then
    expected_server=$(env_value KONFLUX_CLUSTER_SERVER || true)
fi
if [[ -z "$expected_server" ]]; then
    printf 'KONFLUX_CLUSTER_SERVER is required in the environment or in the env file: %s\n' "${ENV_FILE:-none}" >&2
    exit 1
fi
if [[ "$expected_server" != "$cluster_server" ]]; then
    printf 'Cluster server mismatch: expected %s, active %s\n' "$expected_server" "$cluster_server" >&2
    exit 1
fi

printf 'KUBECONFIG=%s\n' "$KUBECONFIG"
printf 'CLUSTER_SERVER=%s\n' "$cluster_server"
printf 'ENV_FILE=%s\n' "$ENV_FILE"
