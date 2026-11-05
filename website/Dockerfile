FROM node:24-bookworm-slim@sha256:c86ecead9e55a5210ac1727de38a4ca840bf03dcaf34dc9749addbf383b1920c

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ENV ASTRO_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false
WORKDIR /website
RUN chown node:node /website
USER node
COPY --chown=node:node package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY --chown=node:node . .
EXPOSE 4321
CMD ["sh", "-ec", "npm run check && npm run preview"]
