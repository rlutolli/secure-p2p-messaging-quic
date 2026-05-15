FROM dorowu/ubuntu-desktop-lxde-vnc:latest

# Fix GPG issues by removing problematic third-party repos (like Chrome)
RUN rm -f /etc/apt/sources.list.d/google-chrome.list

# Install modern Go and build dependencies
RUN apt-get update && \
    apt-get install -y software-properties-common && \
    add-apt-repository ppa:longsleep/golang-backports && \
    apt-get update && \
    apt-get install -y golang-1.21 git build-essential && \
    ln -s /usr/lib/go-1.21/bin/go /usr/local/bin/go && \
    rm -rf /var/lib/apt/lists/*

# Pre-set the environment to make 'go' available
ENV GOPATH=/root/go
ENV PATH=$PATH:/usr/local/go/bin:$GOPATH/bin
