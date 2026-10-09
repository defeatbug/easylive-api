# PROXY_URL=${PROXY_URL:-socks5://host.docker.internal:10808}
PROXY_URL=${PROXY_URL:-http://host.docker.internal:10809}

pe() {
    export HTTP_PROXY=${PROXY_URL} HTTPS_PROXY=${PROXY_URL} http_proxy=${PROXY_URL} https_proxy=${PROXY_URL}
}

p() {
    HTTP_PROXY=${PROXY_URL} HTTPS_PROXY=${PROXY_URL} http_proxy=${PROXY_URL} https_proxy=${PROXY_URL} $*
}

pd() {
    unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY
}

# 常用别名
alias ll='ls -alh'
alias gs='git status -sb'
alias wswatch='tcpdump -i any -n udp port ${MEDIA_UDP_PORT:-7881}'
