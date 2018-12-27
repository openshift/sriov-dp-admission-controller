FROM rhel:rhel7

ADD . /usr/src/sriov-dp-admission-controller

ENV HTTP_PROXY $http_proxy
ENV HTTPS_PROXY $https_proxy
ENV INSTALL_PKGS "git golang make"
RUN yum install -y $INSTALL_PKGS && \
    rpm -V $INSTALL_PKGS && \
    cd /usr/src/sriov-dp-admission-controller && \
    make clean && \
    make build && \
    cp build/webhook /usr/bin/ && \
    cp build/webhook_installer /usr/bin/ && \
    yum autoremove -y $INSTALL_PKGS && \
    yum clean all && \
    rm -rf /tmp/*

WORKDIR /

LABEL io.k8s.display-name="SRIOV Device Plugin Admission Controller"

CMD ["webhook"]
