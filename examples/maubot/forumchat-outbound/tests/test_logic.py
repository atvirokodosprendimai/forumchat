"""Unit tests for forumchat_outbound.logic — stdlib + pytest only, no maubot imports.

Imports logic.py directly via importlib to avoid triggering the package
__init__.py which depends on maubot/mautrix (not available in test env).
"""

import importlib.util
import os

import pytest

# Load logic.py directly, bypassing the package __init__ and maubot deps.
_LOGIC_PATH = os.path.join(
    os.path.dirname(__file__), "..", "forumchat_outbound", "logic.py"
)
_spec = importlib.util.spec_from_file_location("forumchat_outbound_logic", _LOGIC_PATH)
_module = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_module)

should_relay = _module.should_relay
build_text = _module.build_text

ROOM = "!testroom:example.org"
SENDER = "@user:example.org"
BOT = "@bot.maubot:example.org"
ROOMS = [ROOM]
IGNORE = [BOT]
MSGTYPES = ["m.text", "m.emote"]


# ---------------------------------------------------------------------------
# should_relay
# ---------------------------------------------------------------------------


class TestShouldRelayAccepts:
    def test_text_message_relayed(self):
        assert should_relay(ROOM, SENDER, "m.text", "hello", ROOMS, IGNORE, MSGTYPES)

    def test_emote_message_relayed(self):
        assert should_relay(ROOM, SENDER, "m.emote", "waves", ROOMS, IGNORE, MSGTYPES)

    def test_body_with_surrounding_spaces_relayed(self):
        assert should_relay(ROOM, SENDER, "m.text", "  hi  ", ROOMS, IGNORE, MSGTYPES)


class TestShouldRelayRejects:
    def test_empty_rooms_list(self):
        assert not should_relay(ROOM, SENDER, "m.text", "hi", [], IGNORE, MSGTYPES)

    def test_room_not_in_list(self):
        assert not should_relay(
            "!other:example.org", SENDER, "m.text", "hi", ROOMS, IGNORE, MSGTYPES
        )

    def test_sender_in_ignore_list(self):
        assert not should_relay(ROOM, BOT, "m.text", "hi", ROOMS, IGNORE, MSGTYPES)

    def test_msgtype_not_allowed(self):
        assert not should_relay(
            ROOM, SENDER, "m.image", "hi", ROOMS, IGNORE, MSGTYPES
        )

    def test_empty_body(self):
        assert not should_relay(ROOM, SENDER, "m.text", "", ROOMS, IGNORE, MSGTYPES)

    def test_whitespace_only_body(self):
        assert not should_relay(
            ROOM, SENDER, "m.text", "   \t\n", ROOMS, IGNORE, MSGTYPES
        )

    def test_multiple_ignore_senders(self):
        ignore = [BOT, "@another:example.org"]
        assert not should_relay(
            ROOM, "@another:example.org", "m.text", "hi", ROOMS, ignore, MSGTYPES
        )


# ---------------------------------------------------------------------------
# build_text
# ---------------------------------------------------------------------------


class TestBuildText:
    def test_default_format(self):
        assert build_text("Alice", "hello world", "{sender}: {body}") == "Alice: hello world"

    def test_custom_format(self):
        assert build_text("Bob", "bye", "[{sender}] {body}") == "[Bob] bye"

    def test_body_only_format(self):
        assert build_text("Alice", "test", "{body}") == "test"

    def test_unknown_placeholder_falls_back(self):
        assert build_text("Alice", "hi", "{nope}") == "Alice: hi"

    def test_unknown_and_known_placeholder_falls_back(self):
        assert build_text("Alice", "hi", "{sender} {nope}") == "Alice: hi"

    def test_empty_format_returns_empty_string(self):
        # No placeholders, no error; format() returns the literal empty string.
        assert build_text("Alice", "hi", "") == ""

    def test_positional_placeholder_falls_back(self):
        # {0} causes IndexError when called with keyword-only args.
        assert build_text("Alice", "hi", "{0}") == "Alice: hi"
