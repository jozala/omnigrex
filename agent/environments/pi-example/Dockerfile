# Build and publish outside Omnigrex using an exact qualified Pi golden digest.
ARG OMNIGREX_GOLDEN_IMAGE
FROM ${OMNIGREX_GOLDEN_IMAGE}

USER root
ARG EXTRA_DEB_PACKAGE
RUN test -n "$EXTRA_DEB_PACKAGE" && \
    apt-get update && apt-get install -y --no-install-recommends "$EXTRA_DEB_PACKAGE" && \
    rm -rf /var/lib/apt/lists/*
USER 10001:10001
WORKDIR /workspace
