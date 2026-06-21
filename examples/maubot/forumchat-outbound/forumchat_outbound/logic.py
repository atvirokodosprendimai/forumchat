"""Pure relay logic — no maubot/mautrix imports so pytest can run standalone."""

from __future__ import annotations


def should_relay(
    room_id: str,
    sender: str,
    msgtype: str,
    body: str,
    rooms: list,
    ignore_senders: list,
    allowed_msgtypes: list,
) -> bool:
    """Return True only when all relay conditions are satisfied.

    Conditions (all must hold):
    - rooms is non-empty and room_id is listed in rooms
    - sender is not in ignore_senders
    - msgtype is in allowed_msgtypes
    - body is a non-empty, non-whitespace-only string
    """
    if not rooms:
        return False
    if room_id not in rooms:
        return False
    if sender in ignore_senders:
        return False
    if msgtype not in allowed_msgtypes:
        return False
    if not body or not body.strip():
        return False
    return True


def build_text(sender_name: str, body: str, fmt: str) -> str:
    """Format a relay message using the configured format string.

    Falls back to the default "{sender}: {body}" pattern on any
    KeyError or IndexError caused by an unknown placeholder in fmt.
    """
    try:
        return fmt.format(sender=sender_name, body=body)
    except (KeyError, IndexError):
        return f"{sender_name}: {body}"
