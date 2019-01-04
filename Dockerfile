FROM openshift/origin-release:golang-1.10 as builder

ADD . /usr/src/sriov-dp-admission-controller

WORKDIR /usr/src/sriov-dp-admission-controller
RUN make clean && \
    make build

WORKDIR /

FROM openshift/origin-base
COPY --from=builder /usr/src/sriov-dp-admission-controller/build/webhook /usr/bin/
COPY --from=builder /usr/src/sriov-dp-admission-controller/build/webhook_installer /usr/bin/

LABEL io.k8s.display-name="SRIOV Device Plugin Admission Controller"

CMD ["webhook"]
