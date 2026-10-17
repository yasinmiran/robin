# The client for remotejump e2e: Playwright, Google Chrome stable and OpenSSH.
# Chrome stable because it is what users run, and H.264 decode in Chromium
# builds depends on how they were built (selftest reports both).
# Build context: e2e. PW_VERSION must match @playwright/test in package.json.
ARG PW_VERSION=1.64.1
FROM mcr.microsoft.com/playwright:v${PW_VERSION}-noble
ARG PW_VERSION

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-client netcat-openbsd \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /work
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund \
 && test "$(node -p 'require("@playwright/test/package.json").version')" = "$PW_VERSION"
RUN npx playwright install --with-deps chrome \
 && rm -rf /var/lib/apt/lists/* \
 && google-chrome --version
