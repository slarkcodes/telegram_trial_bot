#!/usr/bin/env bash
set -euo pipefail

REPO_DIR=${1:-/opt/telegram_trial_bot}
SERVICE_NAME=${2:-telegram_trial_bot}

cd "$REPO_DIR"

echo "Pulling latest code..."
git pull --ff-only

echo "Building..."
go mod tidy
go build -o telegram_trial_bot

echo "Restarting service..."
sudo systemctl restart "$SERVICE_NAME"

sudo systemctl status "$SERVICE_NAME" --no-pager
