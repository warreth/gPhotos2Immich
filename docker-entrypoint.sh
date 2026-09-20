#!/bin/sh
set -e

# If running as root, fix permissions and drop privileges
if [ "$(id -u)" = "0" ]; then
    # Fix ownership of mounted config file and data directory
    if [ -f /app/config.json ]; then
        chown app:app /app/config.json 2>/dev/null || true
    fi

    if [ -d /app/data ]; then
        chown -R app:app /app/data 2>/dev/null || true
    fi

    # Drop to app user and exec the main command
    exec su-exec app "$@"
else
    # Already running as non-root (e.g. user: 1000:1000 in compose)
    # Just run the command directly
    exec "$@"
fi
