# --- Stage 1: desktopd (stream server, control API, agent API), wallpaper (the
#     one-shot wallpaper setter) and keeper (undo history and audit log, run
#     as its own compose service), all static Go binaries ---
FROM golang:1.25 AS desktopd
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/desktopd . && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wallpaper ./cmd/wallpaper && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/keeper ./cmd/keeper

# --- Stage 2: Helium (Chromium-based browser), signature-checked ---
FROM ubuntu:26.04 AS helium
ARG UBUNTU_MIRROR=archive.ubuntu.com
ARG HELIUM_VERSION=0.19.3.1
COPY helium/signing-key.asc /tmp/key.asc
RUN sed -i "s|archive.ubuntu.com|$UBUNTU_MIRROR|g" /etc/apt/sources.list.d/ubuntu.sources && \
    apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils gpg gpgv && \
    url=https://github.com/imputnet/helium-linux/releases/download/$HELIUM_VERSION/helium-$HELIUM_VERSION-x86_64_linux.tar.xz && \
    curl -fsSL "$url" -o /tmp/h.tar.xz && curl -fsSL "$url.asc" -o /tmp/h.tar.xz.asc && \
    gpg --dearmor < /tmp/key.asc > /tmp/key.gpg && \
    gpgv --keyring /tmp/key.gpg /tmp/h.tar.xz.asc /tmp/h.tar.xz && \
    mkdir /opt/helium && tar -xJf /tmp/h.tar.xz -C /opt/helium --strip-components=1

# --- Stage 3: the desktop ---
FROM ubuntu:26.04
# Ubuntu mirror host; override if archive.ubuntu.com is slow from your network
ARG UBUNTU_MIRROR=archive.ubuntu.com
ENV DEBIAN_FRONTEND=noninteractive

# Headless X server, XFCE's window manager, panel and dock, Files and
# Terminal, and the libraries Helium needs. tzdata (TZ) and the fonts make
# the browser look like an ordinary desktop to websites: Liberation (Arial,
# Times, Courier metrics), Noto for most scripts, CJK, and colour emoji.
# No compositor (it multiplies the X server's CPU use while the screen changes). No desktop icons or desktop menu, no
# translations, no xfce4-session/settings. Package lists are kept so
# `apt install` works straight away.
RUN sed -i "s|archive.ubuntu.com|$UBUNTU_MIRROR|g" /etc/apt/sources.list.d/ubuntu.sources && \
    printf 'path-exclude=/usr/share/locale/*\npath-include=/usr/share/locale/en*\n' > /etc/dpkg/dpkg.cfg.d/nolocale && \
    apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tini dbus-x11 xvfb x11-xserver-utils curl git rsync \
        xfwm4 xfce4-panel xfce4-docklike-plugin xfce4-terminal thunar \
        libnss3 libgtk-3-0t64 libgbm1 libasound2t64 libcups2t64 libxss1 \
        adwaita-icon-theme librsvg2-common fonts-inter-variable fonts-dejavu-core \
        tzdata fonts-liberation fonts-noto-core fonts-noto-cjk fonts-noto-color-emoji && \
    fc-cache -s && \
    find /usr/share/themes -mindepth 1 -maxdepth 1 ! -name Default -exec rm -rf {} + && \
    find /usr/share/icons -mindepth 1 -maxdepth 1 ! -name Adwaita ! -name hicolor ! -name default -exec rm -rf {} + && \
    rm -f /usr/share/applications/*.desktop && \
    apt-get clean && rm -rf /var/cache/* /var/log/*

COPY --from=desktopd /out/ /usr/local/bin/

# Helium, run as the unprivileged "browser" user (see helium/helium.sh);
# the build fails if a library it needs is missing
COPY --from=helium /opt/helium /opt/helium
COPY --chmod=755 helium/helium.sh /usr/local/bin/helium
COPY helium/managed.json /etc/chromium/policies/managed/bot-station.json
COPY helium/recommended.json /etc/chromium/policies/recommended/bot-station.json
RUN ! ldd /opt/helium/chrome | grep "not found" && \
    useradd --create-home --uid 1001 --shell /usr/sbin/nologin browser && \
    update-alternatives --install /usr/bin/x-www-browser x-www-browser /usr/local/bin/helium 200

# XFCE defaults, theme (theme/gtk/<name>, picked in settings.ini), launchers
COPY xfconf/ /etc/xdg/xfce4/xfconf/xfce-perchannel-xml/
COPY theme/agent.menu /etc/xdg/menus/xfce-applications.menu
COPY theme/docklike.rc /etc/xdg/xfce4/panel/docklike.rc
COPY theme/gtk/ /usr/share/themes/
COPY theme/settings.ini /etc/gtk-3.0/settings.ini
COPY theme/icons/ /usr/share/icons/
COPY theme/applications/ /usr/share/applications/
COPY theme/wallpaper.jpg /usr/share/backgrounds/wallpaper.jpg
COPY theme/mimeapps.list /etc/xdg/mimeapps.list
# Desktop look and Files bookmarks; entrypoint.sh puts them in the home folder
COPY theme/gtk.css theme/bookmarks /etc/desktop/
COPY --chmod=755 entrypoint.sh /entrypoint.sh

ENV BROWSER=helium LANG=C.UTF-8 NO_AT_BRIDGE=1 GTK_CSD=1 XCURSOR_THEME=Adwaita XCURSOR_SIZE=24
EXPOSE 8443
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
