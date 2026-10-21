#!/bin/bash
set -e
export DISPLAY=:1
rm -f /tmp/.X1-lock /tmp/.X11-unix/X1
# Chromium can't take proxy credentials on the command line (it would ask
# in a dialog on every start): refuse them rather than browse without
case "$BROWSER_PROXY" in *@*)
    echo "BROWSER_PROXY must be scheme://host:port without user:pass (allow this computer's IP at the proxy instead)" >&2
    exit 1;;
esac
# Languages websites see (navigator.languages, Accept-Language). With TZ,
# set them to match where the computer's IP is. A policy, so it also
# replaces what an existing profile has saved.
langs=$(printf '%s' "${BROWSER_LANG:-en-US,en}" | tr -cd 'A-Za-z0-9,-' | sed 's/,*$//; s/^,*//; s/,,*/","/g')
printf '{"ForcedLanguages": ["%s"]}\n' "$langs" > /etc/chromium/policies/managed/languages.json

# 0. The home folder is a volume that outlives the image. Files and app data
#    stay; the desktop's own look and layout are refreshed from the image on
#    every start, so image updates apply.
mkdir -p /root/Documents /root/Downloads /root/.config/gtk-3.0
# The browser runs as the "browser" user: it may pass through /root (not
# list it) to save into /root/Downloads, and to open files the desktop hands
# it by path (images, video, PDFs... see theme/mimeapps.list). It can read
# only files that are world-readable (the default umask makes them so).
chmod 711 /root
chown browser:browser /root/Downloads
# No browser runs yet: drop profile locks left by a container that was killed
# (Chromium refuses a lock from "another computer", i.e. another hostname)
find /home/browser -maxdepth 4 -name 'Singleton*' -delete 2>/dev/null || true
# The browser only ever stops because the container stopped; Chromium counts
# that as a crash and shows "Restore pages?". Mark the last session clean.
for f in /home/browser/.config/net.imput.helium/*/Preferences; do
    [ -f "$f" ] && sed -i 's/"exit_type":"Crashed"/"exit_type":"Normal"/; s/"exited_cleanly":false/"exited_cleanly":true/' "$f" &&
        chown browser:browser "$f"
done
# Helium's built-in uBlock Origin: off unless BROWSER_ADBLOCK=true. Its
# extension process takes ~85-120 MB. It's a component extension (no
# policy reaches it), so it's switched off the way its toggle in
# helium://extensions does it: "disabled by the user" in the profile.
# A new profile gets a Preferences file with just that.
p=/home/browser/.config/net.imput.helium
[ -e "$p/Default/Preferences" ] || { mkdir -p "$p/Default" && echo '{}' > "$p/Default/Preferences"; }
for f in "$p"/*/Preferences; do
    perl -MJSON::PP -e '
        my ($f, $on) = @ARGV; local $/;
        open my $in, "<", $f or exit; my $p = eval { decode_json(<$in>) } or exit; close $in;
        my $e = $p->{extensions}{settings}{blockjmkbacgjkknlgpkjjiijinjdanf} //= {};
        my @r = grep { $_ != 1 } @{ $e->{disable_reasons} // [] };
        $e->{disable_reasons} = $on ? \@r : [1, @r];
        open my $out, ">", "$f.tmp" or exit; print $out encode_json($p); close $out; rename "$f.tmp", $f' \
        "$f" "$([ "${BROWSER_ADBLOCK:-false}" = true ] && echo 1)" || echo "could not set the ad blocker in $f" >&2
done
chown browser:browser "$p" "$p/Default" "$p"/*/Preferences
ln -sf /etc/desktop/gtk.css /root/.config/gtk-3.0/gtk.css
[ -e /root/.config/gtk-3.0/bookmarks ] || cp /etc/desktop/bookmarks /root/.config/gtk-3.0/
rm -rf /root/.config/xfce4/panel \
    /root/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml \
    /root/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml
mkdir -p /tmp/xvfb

# 1. Headless X display, fixed at 720p. Its framebuffer is a memory-mapped
#    file (/tmp/xvfb/Xvfb_screen0) that desktopd streams from.
Xvfb :1 -screen 0 1280x720x24 -fbdir /tmp/xvfb -nolisten tcp -noreset &
xvfb=$!
until [ -S /tmp/.X11-unix/X1 ]; do sleep 0.1; done

# 1b. Give the display a refresh rate. Xvfb reports none, so browsers draw
#     at 60 fps while anything moves; the stream sends at most STREAM_FPS
#     (25) anyway. Drawing at DISPLAY_HZ instead halves the browser's CPU
#     while scrolling (measured: ~45% of a core at 60, ~25% at 25).
hz=${DISPLAY_HZ:-25}
mode="1280x720_$hz"
clock=$(awk -v hz="$hz" 'BEGIN { printf "%.2f", 1584 * 750 * hz / 1e6 }')
{ xrandr --newmode "$mode" "$clock" 1280 1312 1432 1584 720 723 728 750 -hsync +vsync &&
  xrandr --addmode screen "$mode" && xrandr --output screen --mode "$mode"; } ||
  echo "could not set the display to $hz Hz" >&2

# 2. Session bus, window manager. No compositor: it costs several times the
#    X server's CPU whenever the screen changes.
cd /root
eval "$(dbus-launch --sh-syntax)"
xfwm4 --compositor=off &

# 3. Wallpaper (set once, nothing stays running), top bar + dock
wallpaper || echo "wallpaper failed, plain background" >&2
xfce4-panel &

# 4. Stream + control API, HTTPS/WSS on :8443 (streaming starts disabled).
#    The certificate directory is readable (key.pem itself stays 0600), so
#    the MCP container can pin cert.pem from the shared volume.
mkdir -p "${TLS_DIR:-/var/lib/desktopd}" && chmod 755 "${TLS_DIR:-/var/lib/desktopd}"
desktopd &
desktopd=$!

# On docker stop, give the browser a few seconds to save its profile
trap 'kill -TERM $(pgrep -u browser -o -x helium) 2>/dev/null; for i in 1 2 3 4 5; do pgrep -u browser >/dev/null || break; sleep 1; done; exit 0' TERM INT

# Stop the container if the X server or desktopd exits
wait -n $xvfb $desktopd
