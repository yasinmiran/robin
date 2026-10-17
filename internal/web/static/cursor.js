// RemoteCursor shows the remote pointer image on the video. The agent sends
// each image's PNG once; later switches name it by id.
export class RemoteCursor {
  constructor(el) {
    this.el = el;
    this.images = new Map();
  }

  apply(c) {
    if (c.hidden) {
      this.el.style.cursor = 'none';
      return;
    }
    if (c.png) this.images.set(c.id, `url(data:image/png;base64,${c.png}) ${c.hotX ?? 0} ${c.hotY ?? 0}`);
    const image = this.images.get(c.id);
    // An id this page never got an image for keeps the default arrow.
    this.el.style.cursor = image ? `${image}, default` : 'default';
  }
}
