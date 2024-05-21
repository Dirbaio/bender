#!/bin/bash

set -euxo pipefail

HOST=root@git.akiles.xyz
PORT=7022

IMAGE=embassy.dev/ci
docker build -t $IMAGE image
docker save $IMAGE | pv | ssh -p $PORT $HOST -- ctr -n=bender images import /dev/stdin
