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
	for _, k := range []string{"thread_id", "subject", "thread_root", "message_id"} {
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
