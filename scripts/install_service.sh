#!/usr/bin/env bash
set -euo pipefail

REPO_DIR=${1:-/opt/telegram_trial_bot}
SERVICE_NAME=${2:-telegram_trial_bot}
ENV_FILE=${3:-/opt/telegram_trial_bot/.env}

if [ ! -f "$ENV_FILE" ]; then
  cat <<EOF > "$ENV_FILE"
BOT_TOKEN=PASTE_TOKEN_HERE
CHANNEL_ID=-1001234567890
ADMIN_IDS=12345678,98765432
TRIAL_MINUTES=5
DB_PATH=$REPO_DIR/data.db
EOF
  echo "Created $ENV_FILE. Please edit it with real values."
fi

cd "$REPO_DIR"

go mod tidy
go build -o telegram_trial_bot

sudo tee /etc/systemd/system/${SERVICE_NAME}.service > /dev/null <<EOF
[Unit]
Description=Telegram Trial Bot
After=network.target

[Service]
WorkingDirectory=$REPO_DIR
EnvironmentFile=$ENV_FILE
ExecStart=$REPO_DIR/telegram_trial_bot
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME"
sudo systemctl restart "$SERVICE_NAME"

sudo systemctl status "$SERVICE_NAME" --no-pager
