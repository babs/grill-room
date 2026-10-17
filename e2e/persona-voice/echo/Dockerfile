# Chrome for 013-AC35 with its own PipeWire loop: a null sink whose monitor is the default source, so
# what Chrome plays is what its microphone hears. The host's audio session is never touched. It
# serves Playwright's browser protocol; the test connects to it.
FROM mcr.microsoft.com/playwright:v1.64.0-noble@sha256:06a9939e57531807f8d5fd76ce44b53165ffb7d7501d87ab10e285c20b1e971f
RUN apt-get update && apt-get install -y --no-install-recommends pipewire pipewire-pulse wireplumber pulseaudio-utils dbus-x11 \
	&& rm -rf /var/lib/apt/lists/* \
	&& npm install -g playwright@1.64.0
COPY --chmod=755 start.sh /usr/local/bin/start.sh
USER pwuser
ENTRYPOINT ["/usr/local/bin/start.sh"]
