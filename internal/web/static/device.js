// What the device the page runs on offers for input.
//
// hasTouch means a touch screen, with or without a mouse and keys beside
// it: a phone, a tablet or a touch laptop, any of which can be used with no
// keys at hand, so the page offers a button that raises the soft keyboard.
// touchFirst means the main pointer is a finger, as on a phone or a tablet:
// nothing hovers, and focusing the text box raises the soft keyboard, so
// the page neither takes the focus by itself nor tucks the status tab away.
// A touch laptop has the first and not the second.

export const hasTouch = navigator.maxTouchPoints > 0;
export const touchFirst = matchMedia('(pointer: coarse)').matches;
