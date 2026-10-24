#!/bin/sh
# Renders screen.html to screen.png at 2560x1440 with Google Chrome in the
# e2e client image (e2e/client.Containerfile), whose fonts it names. Build
# that image first, for example with e2e/run.sh selftest.
set -eu
dir=$(cd "$(dirname "$0")" && pwd)
image=${ROBIN_E2E_CLIENT_IMAGE:-localhost/robin-e2e-client}
podman run --rm -v "$dir:/work:Z" "$image" google-chrome --headless=new --no-sandbox --disable-gpu \
	--hide-scrollbars --force-device-scale-factor=1 --window-size=2560,1440 \
	--screenshot=/work/screen.png file:///work/screen.html
