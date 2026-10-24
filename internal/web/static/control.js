// What the page does with each control message from the agent. Features
// register their message types from their own module, so none edits
// another's code.

const handlers = new Map();

// onControl registers handler for control messages of type t.
export function onControl(t, handler) {
  if (handlers.has(t)) throw new Error(`control: ${JSON.stringify(t)} handled twice`);
  handlers.set(t, handler);
}

// dispatchControl hands a message to the handler of its type. Types nobody
// handles are ignored, as from a newer agent.
export function dispatchControl(message) {
  handlers.get(message?.t)?.(message);
}
