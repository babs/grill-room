#!/usr/bin/env bash
set -euo pipefail

# Activate instrumentors ONLY when an endpoint is configured: zero overhead when disabled, no
# double-instrumentation when the platform's OTel Operator injects the wrapper itself.
if [[ -n "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" ]]; then
    export OTEL_SERVICE_NAME="${OTEL_SERVICE_NAME:-grill-tts}"
    # Logs stay on stdout as JSON (see Logging); without this the SDK log exporter double-emits.
    export OTEL_LOGS_EXPORTER="${OTEL_LOGS_EXPORTER:-none}"
    exec opentelemetry-instrument python -m grill_tts   # exec → app is PID 1, gets SIGTERM
fi

exec python -m grill_tts
