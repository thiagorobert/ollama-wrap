#!/bin/sh

#set -e

DOCKER_PID=`ps aux | grep "docker run" | awk '{print $2}'`
# Check if DOCKER_PID is non-empty and re-enable 'set -e'
echo "killing docker with PID $DOCKER_PID"
kill -9 $DOCKER_PID
for I in `docker ps -a -q`; do docker stop $I && docker rm $I; done
