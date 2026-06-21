package webhooks

import (
	"encoding/json"
	"testing"
)

// TestEncodePayloadChat covers the chat path: the generic payload carries no
// thread_* keys, and slack/discord stay flat {"text":...}.
func TestEncodePayloadChat(t *testing.T) {
	m := OutboundMsg{
		CommunityID: "c1", ChannelName: "general", Author: "alice", BodyMD: "hello",
	}

	var generic map[string]any
	if err := json.Unmarshal(encodePayload("generic", m), &generic); err != nil {
		t.Fatalf("generic unmarshal: %v", err)
	}
	for _, k := range []string{"community", "channel", "author", "body_md", "created_at"} {
		if _, ok := generic[k]; !ok {
			t.Fatalf("generic payload missing %q: %v", k, generic)
		}
	}
	for _, k := range []string{"thread_id", "subject", "thread_root", "message_id", "attachments"} {
		if _, ok := generic[k]; ok {
			t.Fatalf("chat payload should omit %q: %v", k, generic)
		}
	}

	var slack map[string]string
	if err := json.Unmarshal(encodePayload("slack", m), &slack); err != nil {
		t.Fatalf("slack unmarshal: %v", err)
	}
	if slack["text"] != "[#general] alice: hello" {
		t.Fatalf("slack text = %q", slack["text"])
	}
}

// TestEncodePayloadForum covers the forum path: the generic payload carries the
// thread identity so a bridge can group it into one external thread.
func TestEncodePayloadForum(t *testing.T) {
	m := OutboundMsg{
		CommunityID: "c1", ChannelName: "general", Author: "alice", BodyMD: "first post",
		ThreadID: "t1", Subject: "Deploy chat", ThreadRoot: true,
	}
	var p map[string]any
	if err := json.Unmarshal(encodePayload("generic", m), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p["thread_id"] != "t1" {
		t.Fatalf("thread_id = %v", p["thread_id"])
	}
	if p["subject"] != "Deploy chat" {
		t.Fatalf("subject = %v", p["subject"])
	}
	if p["thread_root"] != true {
		t.Fatalf("thread_root = %v", p["thread_root"])
	}
	// No post id for the opening message — message_id is omitted, not "".
	if _, ok := p["message_id"]; ok {
		t.Fatalf("root payload should omit message_id: %v", p)
	}

	// A reply carries its post id and thread_root=false.
	m.ThreadRoot = false
	m.MessageID = "p9"
	if err := json.Unmarshal(encodePayload("generic", m), &p); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if p["message_id"] != "p9" || p["thread_root"] != false {
		t.Fatalf("reply payload = %v", p)
	}

	// slack/discord ignore thread fields entirely (flat text, single key).
	var slack map[string]string
	if err := json.Unmarshal(encodePayload("slack", m), &slack); err != nil {
		t.Fatalf("slack unmarshal: %v", err)
	}
	if len(slack) != 1 || slack["text"] == "" {
		t.Fatalf("slack payload should be just {text}: %v", slack)
	}
}

// TestEncodePayloadChatReply covers the chat inline-reply identity: a flat
// message omits both keys; a message carries message_key always and reply_to_key
// only when it is a reply; slack/discord ignore both.
func TestEncodePayloadChatReply(t *testing.T) {
	base := OutboundMsg{CommunityID: "c1", ChannelName: "general", Author: "alice", BodyMD: "hi"}

	// Flat send (no keys): both omitted.
	var flat map[string]any
	if err := json.Unmarshal(encodePayload("generic", base), &flat); err != nil {
		t.Fatalf("flat unmarshal: %v", err)
	}
	for _, k := range []string{"message_key", "reply_to_key"} {
		if _, ok := flat[k]; ok {
			t.Fatalf("flat payload should omit %q: %v", k, flat)
		}
	}

	// Non-reply message: message_key present, reply_to_key omitted.
	m := base
	m.MessageKey = "$evtB"
	var p map[string]any
	if err := json.Unmarshal(encodePayload("generic", m), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p["message_key"] != "$evtB" {
		t.Fatalf("message_key = %v", p["message_key"])
	}
	if _, ok := p["reply_to_key"]; ok {
		t.Fatalf("non-reply must omit reply_to_key: %v", p)
	}

	// Reply: both present.
	m.ReplyToKey = "$evtA"
	if err := json.Unmarshal(encodePayload("generic", m), &p); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if p["message_key"] != "$evtB" || p["reply_to_key"] != "$evtA" {
		t.Fatalf("reply payload keys wrong: %v", p)
	}

	// slack stays flat — no reply keys.
	var slack map[string]any
	if err := json.Unmarshal(encodePayload("slack", m), &slack); err != nil {
		t.Fatalf("slack unmarshal: %v", err)
	}
	if len(slack) != 1 || slack["text"] == "" {
		t.Fatalf("slack payload should be just {text}: %v", slack)
	}
}

// TestEncodePayloadAttachments covers the media path: generic carries an
// attachments array when present and omits it when empty; slack/discord never do.
func TestEncodePayloadAttachments(t *testing.T) {
	m := OutboundMsg{
		CommunityID: "c1", ChannelName: "general", Author: "alice", BodyMD: "see pic",
		Attachments: []OutboundAttachment{
			{URL: "https://h.example/uploads/a1?exp=1&sig=x", MIME: "image/png", Name: "shot.png"},
		},
	}
	var p map[string]any
	if err := json.Unmarshal(encodePayload("generic", m), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	a, ok := p["attachments"].([]any)
	if !ok || len(a) != 1 {
		t.Fatalf("attachments missing/short: %v", p["attachments"])
	}
	first := a[0].(map[string]any)
	if first["url"] != m.Attachments[0].URL || first["mime"] != "image/png" || first["name"] != "shot.png" {
		t.Fatalf("attachment fields wrong: %v", first)
	}

	// Empty -> key absent. Fresh map: Unmarshal merges into a reused map.
	m.Attachments = nil
	var empty map[string]any
	if err := json.Unmarshal(encodePayload("generic", m), &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if _, present := empty["attachments"]; present {
		t.Fatalf("attachments should be absent when empty: %v", empty)
	}

	// slack never carries attachments.
	m.Attachments = []OutboundAttachment{{URL: "u", MIME: "image/png", Name: "n"}}
	var slack map[string]any
	if err := json.Unmarshal(encodePayload("slack", m), &slack); err != nil {
		t.Fatalf("slack unmarshal: %v", err)
	}
	if _, present := slack["attachments"]; present {
		t.Fatalf("slack must not carry attachments: %v", slack)
	}
}
