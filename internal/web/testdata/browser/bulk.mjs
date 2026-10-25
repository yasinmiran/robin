// Drives bulk transfers between the page in system Chrome and the agent's
// own peer, which TestBrowserBulk plays. Prints one JSON line of results.
import { chromium } from 'playwright-core';

const [url] = process.argv.slice(2);
if (!url) {
  console.error('usage: node bulk.mjs <page url>');
  process.exit(2);
}

let browser;
try {
  browser = await chromium.launch({ channel: 'chrome' });
} catch {
  browser = await chromium.launch({ executablePath: '/usr/bin/google-chrome' });
}
try {
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  await page.goto(url);
  await page.waitForFunction(() => window.robin?.state === 'connected', null, { timeout: 15000 });
  const result = await page.evaluate(async () => {
    const { bulk } = window.robin;
    const hex = async (blob) => Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())),
      (b) => b.toString(16).padStart(2, '0')).join('');
    const outcome = (p) => p.then(() => 'ok', (e) => `${e.name}: ${e.message}`);

    bulk.registerKind('test', 16 * 1024 * 1024);
    const received = [];
    let arrived;
    const allArrived = new Promise((resolve) => { arrived = resolve; });
    bulk.onBulk('test', (h) => {
      if (h.id === 'refuse') throw new Error('page says no');
      const parts = [];
      return {
        write: (chunk) => { parts.push(chunk.slice()); },
        commit: () => {
          received.push({ header: h, blob: new Blob(parts) });
          if (h.id === 'down') arrived();
        },
        abort: () => {},
      };
    }, { inbound: 1 });

    // Not a multiple of the chunk size, and several times the queue limit.
    const bytes = new Uint8Array(5 * 1024 * 1024 + 7);
    let x = 1;
    for (let i = 0; i < bytes.length; i++) {
      x = (x * 1103515245 + 12345) >>> 0;
      bytes[i] = x >>> 24;
    }
    const up = new Blob([bytes]);
    const started = performance.now();
    const upload = await outcome(bulk.send({ kind: 'test', id: 'up', name: 'noise.bin', mime: 'application/octet-stream' }, up));
    const uploadMs = Math.round(performance.now() - started);
    await allArrived;
    const down = received.find((r) => r.header.id === 'down');

    const refused = await outcome(bulk.send({ kind: 'test', id: 'refuse' }, new Blob(['x'])));
    const stop = new AbortController();
    setTimeout(() => stop.abort(), 300);
    const aborted = await outcome(bulk.send({ kind: 'test', id: 'cancel' }, new Blob([new Uint8Array(8 * 1024 * 1024)]), { signal: stop.signal }));
    const tooLarge = await outcome(bulk.send({ kind: 'test' }, new Blob([new Uint8Array(16 * 1024 * 1024 + 1)])));

    return {
      upload, uploadMs, upSHA256: await hex(up), size: bytes.length,
      down: { header: down.header, sha256: await hex(down.blob), size: down.blob.size },
      refused, aborted, tooLarge,
    };
  });
  result.errors = errors;
  console.log(JSON.stringify(result));
} finally {
  await browser.close();
}
