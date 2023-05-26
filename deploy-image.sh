#!/bin/bash

set -euxo pipefail

HOST=root@192.168.1.4

IMAGE=embassy.dev/ci
docker build -t $IMAGE image
docker save $IMAGE | pv | ssh $HOST -- ctr -n=bender images import /dev/stdin