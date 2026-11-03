# go-judge sandbox + toolchains (§17.2). Toolchains live in this image because go-judge
# binds the container's own /usr, /lib, /lib64 and /bin into every run.
FROM ubuntu:24.04
ARG GO_JUDGE_VERSION=v1.13.3
ARG GO_JUDGE_SHA256=509782f8959987256d99ac6dc8062ec593d776f291401c8e0c28bdc48dcd1880
ARG TESTLIB_COMMIT=f0169a8f75f5027cb20de61e737635efda7932be
RUN apt-get update && apt-get install -y --no-install-recommends \
      g++-13 python3.12 ca-certificates curl \
 && ln -sf /usr/bin/g++-13 /usr/bin/g++ \
 && rm -rf /var/lib/apt/lists/*
# go-judge release binary, verified
RUN curl -fsSLo /usr/local/bin/go-judge \
      "https://github.com/criyle/go-judge/releases/download/${GO_JUDGE_VERSION}/go-judge_${GO_JUDGE_VERSION#v}_linux_amd64" \
 && echo "${GO_JUDGE_SHA256}  /usr/local/bin/go-judge" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/go-judge
# testlib for checkers and managers
RUN mkdir -p /usr/include/testlib \
 && curl -fsSLo /usr/include/testlib/testlib.h \
      "https://raw.githubusercontent.com/MikeMirzayanov/testlib/${TESTLIB_COMMIT}/testlib.h"
# The shared cache volume is mounted here (read-only). go-judge starts first, so this
# directory seeds the named volume: it must already belong to the worker's UID 65532,
# otherwise the worker cannot write to a freshly created volume.
RUN mkdir -p /var/lib/judge-worker/cache && chown 65532:65532 /var/lib/judge-worker/cache \
 && chmod 0750 /var/lib/judge-worker/cache
COPY mount.yaml /etc/go-judge/mount.yaml
ENTRYPOINT ["/usr/local/bin/go-judge"]
