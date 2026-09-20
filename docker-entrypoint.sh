#!/bin/sh
set -e

# Fix ownership of mounted config file and data directory
# This runs as root, then drops to app user
if [ -f /app/config.json ]; then
    chown app:app /app/config.json 2>/dev/null || true
fi

if [ -d /app/data ]; then
    chown -R app:app /app/data 2>/dev/null || true
fi

# Drop to app user and exec the main command
exec su-exec app "$@"
