#!/bin/bash

getent group obsgateway >/dev/null || groupadd -r obsgateway; /bin/true
getent passwd obsgateway >/dev/null || useradd -r -g obsgateway -s /sbin/nologin -c "obsgateway service" obsgateway