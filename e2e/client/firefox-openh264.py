"""Installs the OpenH264 plugin Firefox needs for H.264 in WebRTC.

Firefox downloads Cisco's OpenH264 build on first use; Playwright's Firefox
never does. This fetches the build Firefox's own manifest names, checks it
against the manifest's sha512 and unpacks it where MOZ_GMP_PATH will point.
Firefox takes the plugin's name and version from the last two path
elements, gmp-gmpopenh264/<version>.

    python3 firefox-openh264.py /ms-playwright/firefox-*/firefox/omni.ja DEST
"""

import hashlib
import io
import json
import platform
import sys
import urllib.request
import zipfile

MANIFEST = "chrome/toolkit/content/global/gmp-sources/openh264.json"
PLATFORMS = {"x86_64": "Linux_x86_64-gcc3", "aarch64": "Linux_aarch64-gcc3"}


def main(omni: str, dest: str) -> None:
    manifest = json.loads(zipfile.ZipFile(omni).read(MANIFEST))
    if manifest.get("hashFunction") != "sha512":
        sys.exit(f"unexpected hash function {manifest.get('hashFunction')!r}")
    entry = manifest["vendors"]["gmp-gmpopenh264"]["platforms"][PLATFORMS[platform.machine()]]
    data = urllib.request.urlopen(entry["fileUrl"], timeout=120).read()
    got = hashlib.sha512(data).hexdigest()
    if got != entry["hashValue"]:
        sys.exit(f"{entry['fileUrl']}: sha512 {got}, Firefox expects {entry['hashValue']}")
    zipfile.ZipFile(io.BytesIO(data)).extractall(dest)
    print(f"OpenH264 for Firefox from {entry['fileUrl']} in {dest}")


if __name__ == "__main__":
    main(*sys.argv[1:])
