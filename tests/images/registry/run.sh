#!/bin/sh
set -e
mkdir -p /auth
htpasswd -Bbn "$REG_USER" "$REG_PASSWORD" > /auth/htpasswd
exec registry serve /etc/distribution/config.yml
