# The browser plugin for Thicket: Playwright driving Chromium, one browser
# per active sprig, each behind its own egress proxy. Build from this folder:
#   docker build -t thicket-browser plugins/browser
# Runs on amd64 and arm64 (Raspberry Pi 4/5): Playwright ships Chromium
# for both.
FROM node:22-bookworm-slim

ENV NODE_ENV=production PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
WORKDIR /app
COPY package.json package-lock.json ./
# Only Chromium's headless shell (no Firefox, WebKit or headed Chromium)
# and the system libraries it needs; tini reaps Chromium's child processes.
RUN npm ci --omit=dev \
 && npx playwright install --with-deps --only-shell chromium \
 && apt-get install -y --no-install-recommends tini \
 && rm -rf /var/lib/apt/lists/* /root/.npm /tmp/*
COPY src ./src

# The token file Thicket writes is readable by its group (gid 65532) only.
# The image's own `node` user (uid 1000) runs the plugin.
RUN groupadd -g 65532 thicket && usermod -aG thicket node \
 && mkdir -p /var/lib/thicket-browser/profiles /run/thicket-browser \
 && chown -R node:node /var/lib/thicket-browser /run/thicket-browser \
 && chmod 700 /var/lib/thicket-browser /var/lib/thicket-browser/profiles /run/thicket-browser
USER node
ENV BROWSER_ADDR=:7073 BROWSER_TOKEN_FILE=/plugin-auth/token \
    BROWSER_PROFILES_DIR=/var/lib/thicket-browser/profiles BROWSER_RUN_DIR=/run/thicket-browser
# Sealed (encrypted) profiles only. Open profiles live in /run/thicket-browser,
# which docker-compose.yml mounts as a tmpfs (memory).
VOLUME /var/lib/thicket-browser
EXPOSE 7073
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD node -e "const p=(process.env.BROWSER_ADDR||':7073').split(':').pop();fetch('http://127.0.0.1:'+p+'/health').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--", "node", "/app/src/main.js"]
