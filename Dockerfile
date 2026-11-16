# ========================================================
# Stage: Frontend (Vite)
# ========================================================
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
COPY internal/web/translation /src/internal/web/translation
RUN npm run build

# ========================================================
# Stage: Builder
# ========================================================
FROM golang:1.26-alpine AS builder
WORKDIR /app
ARG TARGETARCH
ARG TARGETVARIANT
ARG PANEL_VERSION

RUN apk --no-cache --update add \
  build-base \
  gcc \
  curl \
  unzip

COPY . .
COPY --from=frontend /src/internal/web/dist ./internal/web/dist

ENV CGO_ENABLED=1
ENV CGO_CFLAGS="-D_LARGEFILE64_SOURCE"
RUN panel_version="$PANEL_VERSION"; \
  case "$panel_version" in v*) panel_version="$(printf '%s' "$panel_version" | cut -c2-)" ;; esac; \
  if [ -n "$panel_version" ]; then \
    go build -ldflags "-w -s -X github.com/Dark-Sky07/phoenix/internal/config.version=$panel_version" -o build/x-ui main.go; \
  else \
    go build -ldflags "-w -s" -o build/x-ui main.go; \
  fi
RUN ./DockerInit.sh "$TARGETARCH" "$TARGETVARIANT"

# ========================================================
# Stage: Final Image of PHOENIX
# ========================================================
FROM alpine
ENV TZ=Asia/Tehran
WORKDIR /app

# Alpine no longer publishes pptpd for current releases; build the pinned upstream source below.
# Omit upstream's optional logwtmp plugin because it requires libc interfaces unavailable on musl.
RUN apk add --no-cache --update \
  ca-certificates \
  tzdata \
  fail2ban \
  openvpn \
  strongswan \
  xl2tpd \
  ppp \
  iptables \
  iproute2 \
  python3 \
  bash \
  curl \
  openssl \
  && apk add --no-cache --virtual .pppvpn-build-deps py3-pip python3-dev build-base autoconf automake \
  && PPTPD_SRC=/tmp/pptpd-src \
  && PPTPD_TARBALL=/tmp/pptpd-1.5.0.tar.gz \
  && curl -fsSL --retry 3 https://codeload.github.com/quozl/pptpd/tar.gz/refs/tags/1.5.0 -o "$PPTPD_TARBALL" \
  && echo 'b758572f579295dadb19eb8f65bd3dff8348a264cba24aca6b57d3afed8ddcaa  /tmp/pptpd-1.5.0.tar.gz' | sha256sum -c - \
  && mkdir -p "$PPTPD_SRC" \
  && tar -xzf "$PPTPD_TARBALL" -C "$PPTPD_SRC" --strip-components=1 \
  && sed -i 's/^subdirs = plugins$/subdirs =/' "$PPTPD_SRC/Makefile.am" \
  && sed -i '/#include "our_getopt.h"/d' "$PPTPD_SRC/pptpd.c" "$PPTPD_SRC/bcrelay.c" \
  && cd "$PPTPD_SRC" \
  && autoreconf -fi \
  && ./configure --prefix=/usr --sbindir=/usr/sbin --sysconfdir=/etc \
  && make -j2 \
  && make install \
  && cd / \
  && rm -rf "$PPTPD_SRC" "$PPTPD_TARBALL" \
  && python3 -m venv /opt/phoenix/pppvpn-sstp \
  && /opt/phoenix/pppvpn-sstp/bin/python -m pip install --no-cache-dir --upgrade pip setuptools wheel \
  && /opt/phoenix/pppvpn-sstp/bin/python -m pip install --no-cache-dir 'sstp-server==0.7.2' \
  && ln -sf /opt/phoenix/pppvpn-sstp/bin/sstpd /usr/local/bin/sstpd \
  && apk del .pppvpn-build-deps

COPY --from=builder /app/build/ /app/
COPY --from=builder /app/DockerEntrypoint.sh /app/
COPY --from=builder /app/x-ui.sh /usr/bin/x-ui
COPY --from=builder /app/x-ui.sh /usr/bin/phoenix
COPY --from=builder /app/internal/web/translation /app/internal/web/translation


# Configure fail2ban
RUN rm -f /etc/fail2ban/jail.d/alpine-ssh.conf \
  && cp /etc/fail2ban/jail.conf /etc/fail2ban/jail.local \
  && sed -i "s/^\[ssh\]$/&\nenabled = false/" /etc/fail2ban/jail.local \
  && sed -i "s/^\[sshd\]$/&\nenabled = false/" /etc/fail2ban/jail.local \
  && sed -i "s/#allowipv6 = auto/allowipv6 = auto/g" /etc/fail2ban/fail2ban.conf

RUN chmod +x \
  /app/DockerEntrypoint.sh \
  /app/x-ui \
  /usr/bin/x-ui \
  /usr/bin/phoenix

ENV XUI_IN_DOCKER="true"
ENV XUI_MAIN_FOLDER="/app"
ENV XUI_ENABLE_FAIL2BAN="true"
ENV XUI_DB_TYPE=""
ENV XUI_DB_DSN=""
EXPOSE 2053
VOLUME [ "/etc/x-ui" ]
CMD [ "./x-ui" ]
ENTRYPOINT [ "/app/DockerEntrypoint.sh" ]
