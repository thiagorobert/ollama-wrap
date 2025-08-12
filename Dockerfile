FROM ubuntu:22.04


RUN apt update
RUN apt install -y \
    python3 \
    curl


# Install a more recent version of Go.
RUN curl -s https://dl.google.com/go/go1.24.6.linux-amd64.tar.gz -o go-install.tar.gz
RUN tar -zxf go-install.tar.gz -C /usr/local/ 
RUN rm go-install.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}"


# Install ollama and pull gpt-oss model.
ENV OLLAMA_ROOT=/ollama
ENV OLLAMA_MODELS=/ollama/models
RUN mkdir -p ${OLLAMA_MODELS}
WORKDIR ${OLLAMA_ROOT}
COPY data/ollama-linux-amd64.tgz ollama-linux-amd64.tgz
COPY data/ollama-install.sh ollama-install.sh
RUN chmod a+rwx ollama-install.sh
RUN ./ollama-install.sh
COPY data/models ${OLLAMA_MODELS}
RUN rm ollama-linux-amd64.tgz
RUN rm ollama-install.sh

# Source root directory.
ENV CODE_ROOT=/go/src/ollama-wrap


# Copy code/files.
COPY main.go ${CODE_ROOT}/main.go
COPY go.mod ${CODE_ROOT}/go.mod
COPY bootstrap.sh ${CODE_ROOT}/bootstrap.sh


# Expose requierd ports. This is not required, it's more of a documentation.
EXPOSE 8080
EXPOSE 8081


# Create logs dir.
ENV LOGS_ROOT=/logs
RUN mkdir ${LOGS_ROOT}
RUN chmod a+rwx ${LOGS_ROOT}


# Bootsrap.
WORKDIR ${CODE_ROOT}
CMD ["/go/src/ollama-wrap/bootstrap.sh"]
