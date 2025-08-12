#!/bin/sh

set -e

LATEST_OLLAMA_WRAP=${1:-`docker images | awk '{ print $1; }' | grep ollama-wrap | head -1`}

echo "Running $LATEST_OLLAMA_WRAP"

docker run --net host -p 8080:8080 -p 8081:8081 -ti --rm $LATEST_OLLAMA_WRAP
