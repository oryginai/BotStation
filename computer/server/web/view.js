// The desktop alone, in its own tab: /view#token=<viewer token>. The token
// sits after "#", so the browser never sends it to a server; it is removed
// from the address bar and history straight away and works only once.
import { connectDesktop } from '/client.js';

const canvas = document.getElementById('screen');
const msg = document.getElementById('msg');
const token = new URLSearchParams(location.hash.slice(1)).get('token');
history.replaceState(null, '', location.pathname);

const show = (text) => { msg.textContent = text; msg.hidden = false; };

if (!token) {
  show('This link has no viewer token. Request a new link to view the desktop.');
} else {
  const url = location.origin.replace(/^http/, 'ws') + '/stream';
  connectDesktop(canvas, url, token, {
    onOpen: () => { msg.hidden = true; },
    onClose: (reason) => show(`Session ended (${reason}). Request a new link to view again.`),
  });
}

document.getElementById('fullscreen').onclick = async () => {
  await document.documentElement.requestFullscreen();
  // Chrome/Edge: let keys like Esc, Alt+Tab and Ctrl+W reach the desktop
  // (hold Esc to leave full screen)
  navigator.keyboard?.lock?.().catch(() => {});
  canvas.focus();
};
