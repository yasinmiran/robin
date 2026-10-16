// Summarises RTCPeerConnection stats into what the HUD and the e2e tests read.
// Bitrate is measured between samples at least MIN_INTERVAL_S apart, so
// callers polling at different rates all read a stable figure.
const MIN_INTERVAL_S = 0.5;

export class StatsSampler {
  #prev = null;
  #kbps = 0;

  async sample(pc) {
    const out = {
      transport: null,
      local: null,
      remote: null,
      framesDecoded: 0,
      frameWidth: 0,
      frameHeight: 0,
      fps: 0,
      kbps: 0,
    };
    if (!pc || pc.connectionState === 'closed') return out;

    const report = await pc.getStats();
    let pair = null;
    let inbound = null;
    report.forEach((s) => {
      if (s.type === 'transport' && s.selectedCandidatePairId) {
        pair = report.get(s.selectedCandidatePairId) ?? pair;
      } else if (s.type === 'inbound-rtp' && s.kind === 'video') {
        inbound = s;
      }
    });

    out.local = endpoint(report.get(pair?.localCandidateId));
    out.remote = endpoint(report.get(pair?.remoteCandidateId));
    out.transport = out.local?.protocol ?? null;

    if (inbound) {
      out.framesDecoded = inbound.framesDecoded ?? 0;
      out.frameWidth = inbound.frameWidth ?? 0;
      out.frameHeight = inbound.frameHeight ?? 0;
      out.fps = Math.round(inbound.framesPerSecond ?? 0);
      const prev = this.#prev?.id === inbound.id ? this.#prev : null;
      const seconds = prev ? (inbound.timestamp - prev.timestamp) / 1000 : 0;
      if (!prev || seconds >= MIN_INTERVAL_S) {
        if (prev) this.#kbps = Math.round(((inbound.bytesReceived - prev.bytes) * 8) / 1000 / seconds);
        this.#prev = { id: inbound.id, timestamp: inbound.timestamp, bytes: inbound.bytesReceived };
      }
      out.kbps = this.#kbps;
    }
    return out;
  }
}

function endpoint(c) {
  if (!c) return null;
  return {
    address: c.address ?? c.ip ?? null,
    port: c.port ?? null,
    protocol: c.protocol ?? null,
    candidateType: c.candidateType ?? null,
  };
}
