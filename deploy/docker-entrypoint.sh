#!/bin/sh
set -e

# Fix data directory permissions when running as root.
# Docker named volumes / host bind-mounts may be owned by root,
# preventing the non-root sub2api user from writing files.
if [ "$(id -u)" = "0" ]; then
    mkdir -p /app/data
    # Use || true to avoid failure on read-only mounted files (e.g. config.yaml:ro)
    chown -R sub2api:sub2api /app/data 2>/dev/null || true
    if [ -n "${SUB2API_BINARY_DIR:-}" ]; then
        case "$SUB2API_BINARY_DIR" in /|'') echo 'invalid managed binary directory' >&2; exit 1;; esac
        mkdir -p "$SUB2API_BINARY_DIR"
        chown sub2api:sub2api "$SUB2API_BINARY_DIR"
    fi
    # Re-invoke this script as sub2api so the flag-detection below
    # also runs under the correct user.
    exec su-exec sub2api "$0" "$@"
fi

# Compatibility: if the first arg looks like a flag (e.g. --help),
# prepend the default binary so it behaves the same as the old
# ENTRYPOINT ["/app/sub2api"] style.
if [ "${1#-}" != "$1" ]; then
    set -- /app/sub2api "$@"
fi

# Keep online updates across container recreation. A deliberately changed
# image replaces the managed binary; restarting the same image preserves it.
if [ -n "${SUB2API_BINARY_DIR:-}" ] && [ "${1##*/}" = "sub2api" ]; then
    image_binary=$1
    managed_binary="$SUB2API_BINARY_DIR/sub2api"
    marker="$SUB2API_BINARY_DIR/.image-sha256"
    image_hash=$(sha256sum "$image_binary" | cut -d ' ' -f 1)
    previous_hash=$(cat "$marker" 2>/dev/null || true)
    if [ ! -f "$managed_binary" ] || { [ -n "$previous_hash" ] && [ "$previous_hash" != "$image_hash" ]; }; then
        staged="$SUB2API_BINARY_DIR/.sub2api-image-$$"
        trap 'rm -f "$staged"' EXIT HUP INT TERM
        cp "$image_binary" "$staged"
        chmod 755 "$staged"
        if [ -f "$managed_binary" ]; then cp "$managed_binary" "$managed_binary.backup"; fi
        mv -f "$staged" "$managed_binary"
        trap - EXIT HUP INT TERM
    fi
    printf '%s\n' "$image_hash" > "$marker.new"
    mv -f "$marker.new" "$marker"
    shift
    set -- "$managed_binary" "$@"
fi

exec "$@"
