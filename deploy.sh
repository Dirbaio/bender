#!/bin/bash

set -euxo pipefail

CGO_ENABLED=0 go build

HOST=root@git.akiles.xyz
PORT=7022

ssh -p $PORT $HOST -- systemctl stop bender
scp -P $PORT bender config.yaml $HOST:/home/ci/
scp -P $PORT bender.service $HOST:/etc/systemd/system/
ssh -p $PORT  $HOST -- 'systemctl daemon-reload; systemctl restart bender'