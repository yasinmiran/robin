// The session's life beyond one peer connection: what the CLI said about
// the desktop being kept, what the page asked for before leaving, and what
// to tell the user when the socket closes.

export class Session {
  #keep = 0;
  #leaving = null;

  // configure takes the CLI's config. keep is how many seconds the host
  // keeps the desktop without a CLI; 0 means it goes with the session.
  configure(config) {
    this.#keep = config?.keep ?? 0;
  }

  // canKeep says whether detaching leaves a desktop to come back to.
  get canKeep() {
    return this.#keep > 0;
  }

  // detach is the signal that ends this CLI but keeps the desktop.
  detach() {
    this.#leaving = 'kept';
    return { type: 'detach' };
  }

  // end is the signal that ends the session, desktop included.
  end() {
    this.#leaving = 'ended';
    return { type: 'bye' };
  }

  // closeReason names the notice for a closed socket: refused when it never
  // opened, else what the page asked for, else a plain close.
  closeReason(opened) {
    if (!opened) return 'refused';
    return this.#leaving ?? 'closed';
  }
}
