#!/usr/bin/env bash
# One-time, idempotent setup of server-only files for the isolated stack.
# Never overwrites an existing .env or config.toml, so manual edits survive
# every deploy. Secrets are generated here and never leave the server.
set -euo pipefail

APP_DIR="${APP_DIR:-/opt/whatomate-mangalore}"
cd "$APP_DIR"
umask 077
mkdir -p uploads audio

rand() { openssl rand -hex "$1"; }

if [ ! -f .env ]; then
  cat > .env <<EOF
TZ=Asia/Kolkata
POSTGRES_USER=whatomate
POSTGRES_PASSWORD=$(rand 16)
POSTGRES_DB=whatomate
WHATOMATE_IMAGE=
EOF
  echo "bootstrap: created .env"
fi

if [ ! -f config.toml ]; then
  # shellcheck disable=SC1091
  set -a; . ./.env; set +a
  admin_password="$(rand 12)"

  cat > config.toml <<EOF
[app]
name = "Whatomate"
environment = "production"
debug = false
encryption_key = "$(rand 32)"

[server]
host = "0.0.0.0"
port = 8080
read_timeout = 30
write_timeout = 30
base_path = ""
allowed_origins = "${ALLOWED_ORIGINS:-http://localhost:8090}"

[database]
host = "db"
port = 5432
user = "${POSTGRES_USER}"
password = "${POSTGRES_PASSWORD}"
name = "${POSTGRES_DB}"
ssl_mode = "disable"
max_open_conns = 25
max_idle_conns = 5
conn_max_lifetime = 300

[redis]
host = "redis"
port = 6379
username = ""
password = ""
db = 0
tls = false

[jwt]
secret = "$(rand 32)"
access_expiry_mins = 15
refresh_expiry_days = 1

[storage]
type = "local"
local_path = "./uploads"

[whatsapp]
webhook_verify_token = "$(rand 16)"
api_version = "v24.0"
base_url = "https://graph.facebook.com"
app_id = ""
app_secret = ""
config_id = ""

[cookie]
domain = ""

[rate_limit]
enabled = true
login_max_attempts = 10
register_max_attempts = 10
refresh_max_attempts = 30
sso_max_attempts = 10
window_seconds = 60
trust_proxy = true
api_max_requests = 200
api_window_seconds = 60

[tts]
piper_binary = "/usr/local/bin/piper"
piper_model = "/opt/piper/models/en_US-lessac-medium.onnx"

[calling]
audio_dir = "./audio"
recording_enabled = false

[default_admin]
email = "admin@admin.com"
password = "${admin_password}"
full_name = "Admin"
EOF

  printf 'email: admin@admin.com\npassword: %s\n' "$admin_password" > INITIAL_ADMIN_CREDENTIALS.txt
  echo "bootstrap: created config.toml (admin credentials in $APP_DIR/INITIAL_ADMIN_CREDENTIALS.txt)"
fi

# Settings the pipeline owns, re-synced on every deploy.
before="$(sha256sum config.toml)"

# CORS origins are required in production.
if [ -n "${ALLOWED_ORIGINS:-}" ]; then
  sed -i "s|^allowed_origins = .*|allowed_origins = \"${ALLOWED_ORIGINS}\"|" config.toml
fi

# Mangaluru Mahanagara Palike grievance bot toggle.
if [ -n "${MCC_ENABLED:-}" ]; then
  if grep -q '^\[mcc\]' config.toml; then
    sed -i "/^\[mcc\]/,/^\[/ s|^enabled = .*|enabled = ${MCC_ENABLED}|" config.toml
  else
    printf '\n[mcc]\nenabled = %s\n' "$MCC_ENABLED" >> config.toml
  fi
fi

# The app only reads config.toml at startup; flag a restart for the deploy step.
[ "$before" = "$(sha256sum config.toml)" ] || touch .restart-required
