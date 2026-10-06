#!/bin/sh
set -eu

certificate_dir=/tmp/nmcp-tls
mkdir -p "$certificate_dir"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -subj /CN=localhost \
  -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
  -keyout "$certificate_dir/key.pem" \
  -out "$certificate_dir/cert.pem" >/dev/null 2>&1
chmod 0600 "$certificate_dir/key.pem"
exec "$@"
