#!/bin/sh

set -e

LATEST_IMAGE=`docker images | awk '{ print $1; }' | grep ollama-wrap | head -1`
aws ecr-public get-login-password --region us-east-1 | docker login --username AWS --password-stdin public.ecr.aws/f0b1x2x3
docker tag $LATEST_IMAGE public.ecr.aws/f0b1x2x3/ollama-wrapper:latest  
docker push public.ecr.aws/f0b1x2x3/ollama-wrapper:latest
