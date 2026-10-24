// What the status tab shows of an attached desktop's private session, from
// the state the agent sends (protocol.Private) and the switches the page
// is waiting on. No DOM here, so node --test can check it.

const SWITCHES = [
  {
    key: 'curtain',
    label: 'Black out host screen',
    on: 'The screen at the host is black; this page still shows it. The host keyboard and mouse still work',
    off: 'Black out the screen at the host while this page still shows it',
  },
  {
    key: 'silent',
    label: 'Quiet host speakers',
    on: 'The speakers at the host are quiet; this page still plays the sound',
    off: 'Keep the speakers at the host quiet while this page still plays the sound',
  },
];

// view lists the switches to show, in order. A switch the desktop cannot
// have is left out; one the page asked for and has no answer to yet is busy.
export function view(state, pending = {}) {
  if (!state) return [];
  return SWITCHES.filter((s) => !state[s.key]?.unsupported).map((s) => {
    const on = Boolean(state[s.key]?.on);
    return {
      key: s.key,
      label: s.label,
      on,
      busy: s.key in pending,
      title: on ? s.on : s.off,
      error: state[s.key]?.error ?? '',
    };
  });
}

// request is the control message that asks the agent for a switch.
export function request(key, on) {
  return { t: key, on: Boolean(on) };
}

// settle drops from pending, a map from switch to the position asked for,
// each request the agent's state answers: the switch is where it was asked
// to be, or it failed.
export function settle(pending, state) {
  for (const [key, want] of Object.entries(pending)) {
    const s = state?.[key];
    if (!s || Boolean(s.on) === want || s.error || s.unsupported) delete pending[key];
  }
  return pending;
}
