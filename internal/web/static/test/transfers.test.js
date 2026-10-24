import test from 'node:test';
import assert from 'node:assert/strict';
import {
  FAILED, FETCHING, KEPT, OFFERED, QUEUED, SAVED, SENDING, Transfers, formatSize, statusOf,
} from '../transfers.js';

test('formatSize writes binary units as the agent does', () => {
  for (const [n, want] of [[0, '0 B'], [1023, '1023 B'], [1024, '1.0 KiB'], [1536, '1.5 KiB'], [256 * 1024 ** 2, '256.0 MiB'], [4 * 1024 ** 3, '4.0 GiB'], [3 * 1024 ** 5, '3072.0 TiB']]) {
    assert.equal(formatSize(n), want);
  }
});

test('an upload runs from queued to kept, with the name the host gave it', () => {
  let changes = 0;
  const list = new Transfers(() => changes++);
  list.upload('u1', 'a.pdf', 1000);
  assert.equal(list.get('u1').state, QUEUED);
  assert.equal(list.waiting(), true);
  assert.equal(statusOf(list.get('u1')), 'Waiting to upload');
  list.sending('u1');
  list.progress('u1', 250);
  assert.equal(statusOf(list.get('u1')), 'Uploading 25%');
  // The host's name can come before the verdict.
  list.saved('u1', 'a (1).pdf');
  list.finished('u1');
  const t = list.get('u1');
  assert.equal(t.state, KEPT);
  assert.equal(t.done, 1000);
  assert.equal(statusOf(t), 'Uploaded as a (1).pdf');
  assert.equal(list.waiting(), false);
  assert.ok(changes >= 5);
});

test('an upload kept under its own name, and one that failed', () => {
  const list = new Transfers();
  list.upload('u1', 'a.pdf', 10);
  list.sending('u1');
  list.finished('u1');
  list.saved('u1', 'a.pdf');
  assert.equal(statusOf(list.get('u1')), 'Uploaded');
  list.upload('u2', 'b.iso', 10);
  list.sending('u2');
  list.failed('u2', 'files: not enough free space');
  assert.equal(list.get('u2').state, FAILED);
  assert.equal(statusOf(list.get('u2')), 'Upload failed: files: not enough free space');
  assert.equal(statusOf({ state: SENDING, size: 0, done: 0 }), 'Uploading');
});

test('a download is fetched only from the offered state, and a failure leaves it on offer', () => {
  const list = new Transfers();
  assert.equal(list.fetch('nope'), false);
  list.offer('d1', 'report.pdf', 2048);
  assert.equal(statusOf(list.get('d1')), 'Offered by the host');
  assert.equal(list.fetching('d1'), false);
  assert.equal(list.waiting(), true);
  assert.equal(list.fetch('d1'), true);
  assert.equal(list.fetching('d1'), true);
  assert.equal(list.fetch('d1'), false, 'a second click while it downloads');
  list.progress('d1', 1024);
  assert.equal(statusOf(list.get('d1')), 'Downloading 50%');
  list.failed('d1', 'bulk transfer cancelled');
  assert.equal(list.get('d1').state, OFFERED);
  assert.equal(statusOf(list.get('d1')), 'Download failed: bulk transfer cancelled');
  assert.equal(list.fetch('d1'), true, 'it can be tried again');
  assert.equal(list.get('d1').error, '');
  list.finished('d1');
  assert.equal(list.get('d1').state, SAVED);
  assert.equal(statusOf(list.get('d1')), 'Downloaded');
  assert.equal(list.waiting(), false);
});

test('an offer heard again is left as it is, and removal is final', () => {
  const list = new Transfers();
  list.offer('d1', 'a', 1);
  list.fetch('d1');
  list.offer('d1', 'a', 1);
  assert.equal(list.get('d1').state, FETCHING);
  list.offer('d2', 'b', 2);
  assert.deepEqual(list.list().map((t) => t.id), ['d1', 'd2']);
  list.remove('d1');
  list.remove('d1');
  assert.equal(list.get('d1'), undefined);
  assert.equal(list.progress('d1', 1), false);
  assert.equal(list.finished('d1'), false);
  assert.equal(list.failed('d1', 'x'), false);
  assert.equal(list.saved('d1', 'x'), false);
  assert.equal(statusOf({ state: 'from-the-future' }), 'from-the-future');
});
