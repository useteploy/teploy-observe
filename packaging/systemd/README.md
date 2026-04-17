# systemd units for Observe

## Install

```bash
# 1. Create service accounts + data dirs
sudo useradd --system --home /var/lib/observe --shell /usr/sbin/nologin observe
sudo useradd --system --home /var/lib/nucleus --shell /usr/sbin/nologin nucleus
sudo mkdir -p /var/lib/observe /var/lib/nucleus/data /var/log/observe /etc/observe
sudo chown observe:observe /var/lib/observe /var/log/observe /etc/observe
sudo chown nucleus:nucleus /var/lib/nucleus /var/lib/nucleus/data

# 2. Drop binaries
sudo install -m 0755 observe /usr/local/bin/observe
sudo install -m 0755 nucleus /usr/local/bin/nucleus

# 3. Secrets / overrides — required in production
sudo tee /etc/observe/observe.env <<'EOF'
OBSERVE_JWT_SECRET=CHANGE_ME_generate_with_openssl_rand_hex_32
OBSERVE_SESSION_SALT=CHANGE_ME
OBSERVE_ADMIN_USER=admin
OBSERVE_ADMIN_PASSWORD=CHANGE_ME
EOF
sudo chmod 0600 /etc/observe/observe.env
sudo chown observe:observe /etc/observe/observe.env

# 4. Install units
sudo install -m 0644 nucleus.service /etc/systemd/system/nucleus.service
sudo install -m 0644 observe.service /etc/systemd/system/observe.service

sudo systemctl daemon-reload
sudo systemctl enable --now nucleus observe

# 5. Watch logs
sudo journalctl -u observe -f
```

Observe will be on `http://localhost:3000`. Add a reverse proxy (Caddy, nginx) for TLS.
