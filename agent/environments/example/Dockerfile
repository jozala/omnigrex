# Build outside Omnigrex with an exact published golden digest and an approved
# Alpine package version, then publish and select the resulting registry digest.
ARG OMNIGREX_GOLDEN_IMAGE
FROM ${OMNIGREX_GOLDEN_IMAGE}

USER root
ARG EXTRA_APK_PACKAGE
RUN test -n "$EXTRA_APK_PACKAGE" && apk add --no-cache "$EXTRA_APK_PACKAGE"
USER 10001:10001
WORKDIR /workspace
