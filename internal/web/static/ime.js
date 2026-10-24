// The keyboard's focus rests in a hidden text box, so that input methods and
// soft keyboards have something to type into. Keys the browser names still
// go out as keys; this module decides what else the text box's events send:
// text an input method composed, and what a soft keyboard typed without
// naming a key, each once.

// MAX_TEXT_BYTES bounds one text message, as protocol.MaxTextBytes does.
export const MAX_TEXT_BYTES = 4096;

// imeOwns reports whether a key event is the text box's to handle, not one
// to send: a key an input method is composing with, which Chrome first
// reports as Process with keyCode 229 before setting isComposing; a dead
// key, which starts a composition; and the Unidentified keys of soft
// keyboards, whose text arrives as input events instead.
export function imeOwns(e) {
  return Boolean(e.isComposing) || e.keyCode === 229 || e.key === 'Process' || e.key === 'Dead' || e.key === 'Unidentified';
}

// fromInput returns the messages for a beforeinput event outside a
// composition: typed text as text, line breaks and deletions as the keys
// that make them. The page cancels every such event, so the text box stays
// empty and nothing is typed twice.
export function fromInput(inputType, data) {
  switch (inputType) {
    case 'insertText':
    case 'insertReplacementText':
      return textMessages(data ?? '');
    case 'insertLineBreak':
    case 'insertParagraph':
      return tap('Enter');
    case 'deleteContentBackward':
    case 'deleteWordBackward':
    case 'deleteSoftLineBackward':
    case 'deleteHardLineBackward':
      return tap('Backspace');
    case 'deleteContentForward':
    case 'deleteWordForward':
    case 'deleteSoftLineForward':
    case 'deleteHardLineForward':
      return tap('Delete');
  }
  // Pastes arrive through the paste event; the rest has no key to send.
  return [];
}

// fromComposition returns the messages for a composition that committed
// data.
export function fromComposition(data) {
  return textMessages(data ?? '');
}

// textMessages cuts text into text messages of at most MAX_TEXT_BYTES of
// UTF-8 each, between characters.
export function textMessages(text) {
  const out = [];
  let part = '';
  let bytes = 0;
  for (const ch of text) {
    const n = utf8Length(ch.codePointAt(0));
    if (bytes + n > MAX_TEXT_BYTES) {
      out.push({ t: 'text', text: part });
      part = '';
      bytes = 0;
    }
    part += ch;
    bytes += n;
  }
  if (part) out.push({ t: 'text', text: part });
  return out;
}

function utf8Length(cp) {
  if (cp < 0x80) return 1;
  if (cp < 0x800) return 2;
  if (cp < 0x10000) return 3;
  return 4;
}

function tap(key) {
  return [
    { t: 'key', key, code: key, down: true, repeat: false },
    { t: 'key', key, code: key, down: false, repeat: false },
  ];
}
