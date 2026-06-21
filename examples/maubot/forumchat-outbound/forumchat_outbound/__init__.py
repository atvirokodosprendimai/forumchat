"""ForumchatOutbound — relay Matrix room messages to a forumchat inbound webhook."""

from __future__ import annotations

from maubot import Plugin, MessageEvent
from maubot.handlers import event
from mautrix.types import EventType
from mautrix.util.config import BaseProxyConfig, ConfigUpdateHelper

from .logic import build_text, should_relay

_CONFIG_KEYS = (
    "target_url",
    "rooms",
    "ignore_senders",
    "allowed_msgtypes",
    "format",
    "use_display_name",
)


class Config(BaseProxyConfig):
    def do_update(self, helper: ConfigUpdateHelper) -> None:
        for key in _CONFIG_KEYS:
            helper.copy(key)


class ForumchatOutbound(Plugin):
    async def start(self) -> None:
        self.config.load_and_update()

    @classmethod
    def get_config_class(cls) -> type[Config]:
        return Config

    @event.on(EventType.ROOM_MESSAGE)
    async def on_message(self, evt: MessageEvent) -> None:
        target_url: str = self.config["target_url"]
        if not target_url:
            return

        msgtype: str = str(evt.content.msgtype)
        body: str = evt.content.body or ""

        if not should_relay(
            room_id=str(evt.room_id),
            sender=str(evt.sender),
            msgtype=msgtype,
            body=body,
            rooms=self.config["rooms"],
            ignore_senders=self.config["ignore_senders"],
            allowed_msgtypes=self.config["allowed_msgtypes"],
        ):
            return

        sender_name = await self._resolve_sender_name(str(evt.sender))
        text = build_text(sender_name, body, self.config["format"])

        await self._post(target_url, text)

    async def _resolve_sender_name(self, mxid: str) -> str:
        """Return display name for mxid, falling back to localpart on any error."""
        if self.config["use_display_name"]:
            try:
                display_name = await self.client.get_displayname(mxid)
                if display_name:
                    return display_name
            except Exception:  # noqa: BLE001
                pass
        return mxid.split(":")[0].lstrip("@")

    async def _post(self, url: str, text: str) -> None:
        """POST text as JSON to the forumchat inbound webhook."""
        try:
            async with self.http.post(url, json={"text": text}) as resp:
                if resp.status >= 300:
                    self.log.warning(
                        "forumchat webhook returned non-2xx status %d for URL %s",
                        resp.status,
                        url,
                    )
        except Exception:
            self.log.exception("Failed to POST to forumchat webhook %s", url)
