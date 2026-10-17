"""One JSON object per line on stdout."""

import logging
import sys

import structlog


def setup_logging(level: str) -> None:
    renderer: structlog.typing.Processor = (
        structlog.dev.ConsoleRenderer() if sys.stdout.isatty() else structlog.processors.JSONRenderer()
    )
    structlog.configure(
        processors=[
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso", utc=True),
            # A traceback becomes one escaped field: one JSON object per line.
            structlog.processors.dict_tracebacks,
            renderer,
        ],
        wrapper_class=structlog.make_filtering_bound_logger(logging.getLevelNamesMapping()[level.upper()]),
    )
