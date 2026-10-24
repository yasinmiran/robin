// The status tab's switches, which several features keep a row of: a small
// track that fills when on, beside a label, with a muted note under the
// row where one needs saying.

// switchButton is the switch for item: its key, label, on, busy and title,
// disabled when it cannot be used, and describedBy, the id of the note that
// says why or how. A disabled switch stays focusable and says so through
// aria-disabled, so its note can be reached; a click on it does nothing.
// onToggle(key) runs on a click of one that can be used.
export function switchButton(item, onToggle) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'switch';
  b.dataset.switch = item.key;
  b.dataset.keys = 'local';
  b.setAttribute('role', 'switch');
  b.setAttribute('aria-checked', String(item.on));
  b.setAttribute('aria-busy', String(Boolean(item.busy)));
  if (item.disabled) b.setAttribute('aria-disabled', 'true');
  if (item.describedBy) b.setAttribute('aria-describedby', item.describedBy);
  if (item.title) b.title = item.title;
  const track = document.createElement('span');
  track.className = 'track';
  track.setAttribute('aria-hidden', 'true');
  const label = document.createElement('span');
  label.textContent = item.label;
  b.append(track, label);
  b.addEventListener('click', () => {
    b.blur(); // keys belong to the remote, not to this switch
    if (!item.disabled) onToggle(item.key);
  });
  return b;
}

// switchNote is a line under a row of switches, which a switch names by id
// in its describedBy.
export function switchNote(id, text) {
  const p = document.createElement('p');
  p.className = 'switch-note';
  p.id = id;
  p.textContent = text;
  return p;
}
