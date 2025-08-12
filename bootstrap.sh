#!/bin/sh

set -e

echo "starting a file server...."
cd ${LOGS_ROOT}
python3 -m http.server 8081 2>&1  > ${LOGS_ROOT}/fileserver.log &
cd -

echo "starting ollama...."
cd ${LOGS_ROOT}
ollama serve  2>&1  > ${LOGS_ROOT}/ollama.log &
sleep 5
cd -


echo "starting ollama-wrap...."
go build -o ollama-wrap
./ollama-wrap  2>&1  > ${LOGS_ROOT}/ollama-wrap.log

