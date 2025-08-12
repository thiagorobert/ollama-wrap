#!/bin/sh

set -e

docker build "$@" . -t ollama-wrap-v`date +"%Y%m%d%H%M%S"`
