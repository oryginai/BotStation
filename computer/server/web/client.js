// Stream client for desktopd. Draws the remote desktop on a canvas and sends
// mouse and keyboard input back.
//
//   import { connectDesktop } from 'https://<host>/client.js';
//   const desktop = connectDesktop(canvas, 'wss://<host>/stream', viewerToken, {
//     onOpen() {}, onClose(reason, clean) {},
//   });
//   desktop.close();
//
// The viewer token goes in the WebSocket subprotocol list, never in the URL.
//
// Clipboard: text copied inside the desktop is written to the local
// clipboard; Ctrl+V / Ctrl+Shift+V / Shift+Insert in the desktop pastes the
// local clipboard (the browser hands it over in a paste event).

export function connectDesktop(canvas, url, token, { onOpen, onClose } = {}) {
  const ctx = canvas.getContext('2d');
  const ws = new WebSocket(url, ['desktop.v1', 'token.' + token]);
  const listeners = new AbortController();
  const on = (type, fn, opts = {}) => canvas.addEventListener(type, fn, { ...opts, signal: listeners.signal });
  let viewOnly = false;
  let frames = Promise.resolve(); // draw frames strictly in order
  ws.binaryType = 'arraybuffer';

  const send = (msg) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
  };

  // The desktop's pointer is hidden there and drawn here. While you move the
  // mouse it is your local cursor, with no network lag. When something else
  // moves it (the agent, another viewer), and always for view-only viewers,
  // it is an image placed at the position the server reports.
  const overlay = document.createElement('img');
  overlay.alt = '';
  overlay.style.cssText = 'position:fixed;pointer-events:none;z-index:2147483647;display:none';
  document.body.append(overlay);
  let shape = null;   // { png, x, y }: image and hot spot
  let remote = null;  // { x, y }: the desktop's pointer
  let sent = null;    // last position we moved it to
  let movedAt = -Infinity;
  const shapeCSS = () => `url(data:image/png;base64,${shape.png}) ${shape.x} ${shape.y}, default`;

  function showLocal() {
    overlay.style.display = 'none';
    if (shape && !viewOnly) canvas.style.cursor = shapeCSS();
  }
  function showRemote() {
    if (!shape || !remote) return;
    const r = canvas.getBoundingClientRect();
    overlay.style.left = `${r.left + remote.x * r.width / canvas.width - shape.x}px`;
    overlay.style.top = `${r.top + remote.y * r.height / canvas.height - shape.y}px`;
    overlay.style.display = '';
    canvas.style.cursor = 'none';
  }
  const remoteShown = () => overlay.style.display !== 'none';
  addEventListener('resize', () => remoteShown() && showRemote(), { signal: listeners.signal });

  ws.onopen = () => {
    canvas.focus();
    onOpen?.();
  };

  ws.onmessage = (e) => {
    if (typeof e.data === 'string') {
      const m = JSON.parse(e.data);
      if (m.type === 'hello') {
        canvas.width = m.width;
        canvas.height = m.height;
        viewOnly = m.view_only;
      } else if (m.type === 'cursor') {
        shape = m;
        overlay.src = `data:image/png;base64,${m.png}`;
        if (remoteShown() || viewOnly) showRemote(); else showLocal();
      } else if (m.type === 'pointer') {
        remote = m;
        // our own moves echo back; ignore them and anything while we move
        const echo = sent && m.x === sent.x && m.y === sent.y;
        if (viewOnly || (!echo && performance.now() - movedAt > 500)) showRemote();
      } else if (m.type === 'clipboard') {
        copyOut(m.text);
      }
      return;
    }
    frames = frames.then(() => drawFrame(e.data)).catch(console.error).then(() => send({ t: 'ack' }));
  };

  ws.onclose = (e) => {
    releaseKeys();
    listeners.abort();
    overlay.remove();
    canvas.style.cursor = '';
    onClose?.(e.reason || (e.code === 1000 ? 'closed' : 'connection lost'), e.code === 1000);
  };

  // frame: 0x01, u16 count, then count x (u16 x, u16 y, u16 w, u16 h, u32 len, JPEG)
  async function drawFrame(buf) {
    const dv = new DataView(buf);
    if (dv.getUint8(0) !== 1) return;
    const tiles = [];
    for (let i = 0, off = 3, n = dv.getUint16(1); i < n; i++) {
      const x = dv.getUint16(off), y = dv.getUint16(off + 2), len = dv.getUint32(off + 8);
      const blob = new Blob([new Uint8Array(buf, off + 12, len)], { type: 'image/jpeg' });
      tiles.push(createImageBitmap(blob).then((bmp) => ({ x, y, bmp })));
      off += 12 + len;
    }
    for (const { x, y, bmp } of await Promise.all(tiles)) {
      ctx.drawImage(bmp, x, y);
      bmp.close();
    }
  }

  // --- input, in desktop pixels ---

  const point = (e) => {
    const r = canvas.getBoundingClientRect();
    return {
      x: Math.round((e.clientX - r.left) * canvas.width / r.width),
      y: Math.round((e.clientY - r.top) * canvas.height / r.height),
    };
  };
  const button = (e) => [1, 2, 3][e.button] || 1;

  let move = null, lastClient = null;
  const moveTo = (p) => { sent = p; send({ t: 'm', ...p }); };
  on('pointermove', (e) => {
    if (viewOnly) return;
    // browsers fire moves for a still mouse when the page under it changes
    // (our overlay showing, the cursor style changing); only real ones count
    if (lastClient && e.clientX === lastClient.x && e.clientY === lastClient.y) return;
    lastClient = { x: e.clientX, y: e.clientY };
    movedAt = performance.now();
    if (remoteShown()) showLocal();
    if (!move) requestAnimationFrame(() => { moveTo(move); move = null; });
    move = point(e);
  });
  on('pointerdown', (e) => {
    canvas.focus();
    retryCopy();
    if (viewOnly) return;
    canvas.setPointerCapture(e.pointerId);
    movedAt = performance.now();
    moveTo(point(e));
    send({ t: 'b', b: button(e), d: true });
    e.preventDefault();
  });
  on('pointerup', (e) => !viewOnly && send({ t: 'b', b: button(e), d: false }));
  on('contextmenu', (e) => e.preventDefault());

  let wheelX = 0, wheelY = 0;
  on('wheel', (e) => {
    e.preventDefault();
    if (viewOnly) return;
    const unit = e.deltaMode === 1 ? 1 : 40; // lines or pixels per notch
    wheelX += e.deltaX / unit;
    wheelY += e.deltaY / unit;
    while (Math.abs(wheelY) >= 1) { send({ t: 'w', dx: 0, dy: Math.sign(wheelY) }); wheelY -= Math.sign(wheelY); }
    while (Math.abs(wheelX) >= 1) { send({ t: 'w', dx: Math.sign(wheelX), dy: 0 }); wheelX -= Math.sign(wheelX); }
  }, { passive: false });

  // --- clipboard ---

  // Desktop -> local. Browsers may refuse without a recent click or key
  // press; then it is retried on the next one.
  let pendingCopy = null;
  function copyOut(text) {
    pendingCopy = text;
    navigator.clipboard?.writeText(text).then(() => { pendingCopy = null; }, () => {});
  }
  const retryCopy = () => pendingCopy !== null && copyOut(pendingCopy);

  // Local -> desktop. A paste shortcut is not sent straight away: the
  // browser first fires a paste event with the local clipboard, which goes
  // to the desktop, and then the key that makes the app paste it.
  const isPaste = (e) => (e.ctrlKey && e.code === 'KeyV') || (e.shiftKey && e.code === 'Insert');
  let paste = null; // { code, up, timer } while waiting for the paste event
  function sendPasteKey() {
    if (!paste) return;
    clearTimeout(paste.timer);
    send({ t: 'k', c: paste.code, d: true });
    if (paste.up) send({ t: 'k', c: paste.code, d: false });
    paste = null;
  }
  on('paste', (e) => {
    e.preventDefault();
    const text = e.clipboardData?.getData('text/plain');
    if (text && !viewOnly) send({ t: 'clip', text });
    sendPasteKey();
  });

  const held = new Set();
  on('keydown', (e) => {
    retryCopy();
    if (viewOnly) return;
    if (e.repeat) { e.preventDefault(); return; } // the desktop repeats held keys itself
    held.add(e.code);
    if (isPaste(e)) { // let the browser fire 'paste'; send the key after it
      paste = { code: e.code, up: false, timer: setTimeout(sendPasteKey, 300) };
      return;
    }
    e.preventDefault();
    send({ t: 'k', c: e.code, d: true });
  });
  on('keyup', (e) => {
    if (viewOnly) return;
    e.preventDefault();
    held.delete(e.code);
    if (paste && paste.code === e.code) { paste.up = true; return; }
    send({ t: 'k', c: e.code, d: false });
  });
  on('blur', releaseKeys);

  function releaseKeys() {
    for (const c of held) send({ t: 'k', c, d: false });
    held.clear();
  }

  return { close: () => ws.close(1000, 'viewer closed') };
}
