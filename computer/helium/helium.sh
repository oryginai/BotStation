#!/bin/sh
# Every browser start (dock, menu, links, the agent API) goes through here.
# Chromium-based browsers refuse to sandbox themselves as root, so the
# browser runs as the unprivileged "browser" user, with its full sandbox
# (user namespaces, allowed by compose's seccomp.json). The desktop itself
# stays root.
H=/opt/helium/helium-wrapper
# The agent API's browser part (desktopd, /agent/browser/...) drives the
# browser over the DevTools protocol. Chromium binds the port to 127.0.0.1:
# only processes inside the container can reach it.
if [ "${BROWSER_AUTOMATION:-true}" = true ] && [ "${AGENT_API:-true}" = true ]; then
    set -- --remote-debugging-port=9222 "$@"
fi
# The browser's own language: the first of BROWSER_LANG (the list websites
# see is a policy, written by entrypoint.sh)
lang=${BROWSER_LANG:-en-US,en}
set -- --lang="${lang%%,*}" "$@"
# WebGL in software (SwiftShader), as nearly every real browser has WebGL.
# Only WebGL goes through it: the page itself is still drawn by the
# software compositor, which is far cheaper (measured while scrolling: ~30%
# of a core either way; SwiftShader compositing would take ~200%).
if [ "${BROWSER_WEBGL:-true}" = true ]; then
    set -- --use-angle=swiftshader --enable-unsafe-swiftshader --disable-gpu-compositing "$@"
else
    set -- --disable-gpu "$@"
fi
# A fixed egress proxy, host:port (http://, socks5://...). WebRTC then
# uses it too, so it doesn't reveal the computer's own IP.
if [ -n "$BROWSER_PROXY" ]; then
    set -- --proxy-server="$BROWSER_PROXY" --force-webrtc-ip-handling-policy=disable_non_proxied_udp "$@"
fi
if [ "$(id -u)" = 0 ]; then
    exec setpriv --reuid=browser --regid=browser --init-groups --inh-caps=-all \
        env -i HOME=/home/browser USER=browser LOGNAME=browser PATH=/usr/local/bin:/usr/bin:/bin \
        DISPLAY="${DISPLAY:-:1}" LANG="${LANG:-C.UTF-8}" GTK_CSD="${GTK_CSD:-1}" ${TZ:+"TZ=$TZ"} \
        XCURSOR_THEME="${XCURSOR_THEME:-Adwaita}" XCURSOR_SIZE="${XCURSOR_SIZE:-24}" \
        "$H" --start-maximized "$@"
fi
exec "$H" --start-maximized "$@"
