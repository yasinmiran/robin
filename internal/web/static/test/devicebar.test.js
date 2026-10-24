import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeElement, installDom } from './dom.js';
import { offerDevices } from '../devicebar.js';

// sectionsOverlay keeps its sections by name and refuses a name twice, as
// Overlay.addSection does, until the section is removed.
function sectionsOverlay() {
  const sections = new Map();
  return {
    sections,
    addSection(name) {
      if (sections.has(name)) throw new Error(`overlay: section ${name} added twice`);
      const section = new FakeElement('section');
      section.remove = () => sections.delete(name);
      sections.set(name, section);
      return section;
    },
  };
}

const sender = () => ({ replaceTrack: async () => {} });
const peer = () => ({ addTransceiver: () => ({ sender: sender(), setCodecPreferences() {} }) });

test('the switches go with their peer connection, and the next one gets its own', async () => {
  installDom();
  // The camera's line asks for codecs; under node there are none.
  globalThis.RTCRtpSender = globalThis.RTCRtpReceiver = { getCapabilities: () => ({ codecs: [] }) };
  const overlay = sectionsOverlay();
  const config = { mic: true, camera: true };

  const first = offerDevices(peer(), { sender: sender() }, config, overlay);
  assert.deepEqual(Object.keys(first.switches), ['mic', 'camera']);
  await first.stop();
  assert.equal(overlay.sections.size, 0, 'the switches stayed after their peer connection went');

  // A reconnect builds the next peer connection from the next config.
  const second = offerDevices(peer(), { sender: sender() }, config, overlay);
  assert.equal(overlay.sections.get('devices').children.length, 2);
  assert.equal(second.switches.mic.state, 'off');
});
