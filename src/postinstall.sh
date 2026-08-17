#!/bin/bash

mkdir -p /var/lib/obsgateway
chown obsgateway:obsgateway /var/lib/obsgateway 2>/dev/null || true

if [ $1 -eq 1 ] && [ -x "/usr/lib/systemd/systemd-update-helper" ]; then
    # Initial installation
    /usr/lib/systemd/systemd-update-helper install-system-units obsgateway.service || :
fi
