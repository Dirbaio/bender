#!/bin/bash

set -euxo pipefail

CGO_ENABLED=0 go build

HOST=root@192.168.1.4

ssh $HOST -- systemctl stop bender
scp bender config.yaml $HOST:/home/ci/
scp bender.service $HOST:/etc/systemd/system/
ssh $HOST -- 'systemctl daemon-reload; systemctl restart bender'