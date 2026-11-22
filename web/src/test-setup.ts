// jsdom plays no media: it reports each call of a media element's load() or
// pause() as not implemented. No test plays media, so both do nothing here; a
// test that checks the calls replaces them with spies.
HTMLMediaElement.prototype.load = () => {}
HTMLMediaElement.prototype.pause = () => {}
