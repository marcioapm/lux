#!/bin/sh
# ROLE=web: serve NAME on :80.  ROLE=dns: answer RECORDS ("name=ip,name=ip").
set -e
case "$ROLE" in
web)
	mkdir -p /www && echo "$NAME" > /www/index.html
	exec httpd -f -p 80 -h /www ;;
dns)
	args=""
	for r in $(echo "$RECORDS" | tr , ' '); do args="$args --address=/${r%%=*}/${r#*=}"; done
	exec dnsmasq -k --no-resolv --no-hosts --log-queries --log-facility=- $args ;;
esac
