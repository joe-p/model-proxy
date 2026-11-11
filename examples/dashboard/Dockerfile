# syntax=docker/dockerfile:1

# Build stage: type-check and compile the TypeScript sources to plain JS.
FROM node:24-alpine AS build
WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json ./
COPY src ./src
RUN npx tsc --noEmit false --outDir dist

# Runtime stage: the collector has no runtime dependencies, so it only needs
# Node and the compiled output.
FROM node:24-alpine

ENV NODE_ENV=production
WORKDIR /app

COPY --from=build /app/package.json ./
COPY --from=build /app/dist ./dist
COPY public ./public

# Keep SQLite on a volume so restarts and image upgrades preserve history.
RUN mkdir -p /data && chown node:node /data
VOLUME ["/data"]

USER node
EXPOSE 8787

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8787/healthz || exit 1

# Bind to all interfaces: the default 127.0.0.1 is unreachable from outside
# the container. Publish the port only on a trusted network, or put a reverse
# proxy with authentication in front of it.
ENTRYPOINT ["node", "dist/index.js"]
CMD ["--host", "0.0.0.0", "--port", "8787", "--db", "/data/usage.db"]
