#!/bin/sh

set -e

LATEST_IMAGE=${1:-`docker images | awk '{ print $1; }' | grep ollama-wrap | head -1`}

echo "Running latest image: $LATEST_IMAGE"

docker run -p 8080:8080 -ti --rm --entrypoint /bin/bash $LATEST_IMAGE
