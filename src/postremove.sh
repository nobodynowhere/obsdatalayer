#!/bin/bash

if [ $1 -ge 1 ] && [ -x "/usr/lib/systemd/systemd-update-helper" ]; then
    # Package upgrade, not removal
    /usr/lib/systemd/systemd-update-helper mark-restart-system-units obsgateway.service || :
fi
