package community

import (
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/config"
)

func ptrBool(b bool) *bool { return &b }

func TestResolveRAG_SelfHostUsesEnv(t *testing.T) {
	cfg := config.Config{SAAS: false, RAGEnabled: true, RAGEmbedModel: "bge-m3", RAGEmbedDim: 1024}
	// Settings are populated but MUST be ignored in self-hosted mode.
	s := Settings{CommunityID: "c1", RAGEnabled: ptrBool(false), RAGEmbedModel: "other", RAGEmbedDim: 768}
	got := ResolveRAG(s, cfg)
	if !got.Enabled || got.EmbedModel != "bge-m3" || got.EmbedDim != 1024 {
		t.Fatalf("self-host must use env, got %+v", got)
	}
	if got.QdrantColl != "" {
		t.Fatalf("self-host has no per-community collection, got %q", got.QdrantColl)
	}
}

func TestResolveRAG_SaaSOverrideAndDefaultCollection(t *testing.T) {
	cfg := config.Config{SAAS: true, RAGEnabled: true, RAGEmbedModel: "bge-m3", RAGEmbedDim: 1024}
	s := Settings{CommunityID: "c1", RAGEnabled: ptrBool(true), RAGEmbedModel: "e5-large", RAGEmbedDim: 4096}
	got := ResolveRAG(s, cfg)
	if !got.Enabled || got.EmbedModel != "e5-large" || got.EmbedDim != 4096 {
		t.Fatalf("SaaS override must win, got %+v", got)
	}
	if got.QdrantColl != "forumchat_c1" {
		t.Fatalf("default collection = %q, want forumchat_c1", got.QdrantColl)
	}
}

func TestResolveRAG_KillSwitch(t *testing.T) {
	cfg := config.Config{SAAS: true, RAGEnabled: false} // platform kill-switch off
	s := Settings{CommunityID: "c1", RAGEnabled: ptrBool(true)}
	if ResolveRAG(s, cfg).Enabled {
		t.Fatal("global RAG_ENABLED=false must disable regardless of community override")
	}
}

func TestResolveRAG_SaaSOptInDefaultOff(t *testing.T) {
	cfg := config.Config{SAAS: true, RAGEnabled: true}
	if ResolveRAG(Settings{CommunityID: "c1"}, cfg).Enabled {
		t.Fatal("SaaS RAG is opt-in: unset community must be off")
	}
}

func TestEffectiveAIEnabled(t *testing.T) {
	if EffectiveAIEnabled(Settings{}, config.Config{AIEnabled: false}) {
		t.Fatal("kill-switch off => AI off")
	}
	if !EffectiveAIEnabled(Settings{}, config.Config{SAAS: false, AIEnabled: true}) {
		t.Fatal("self-host with AI_ENABLED on => AI on")
	}
	if EffectiveAIEnabled(Settings{AIEnabled: ptrBool(false)}, config.Config{SAAS: true, AIEnabled: true}) {
		t.Fatal("SaaS community master toggle off => AI off")
	}
	if !EffectiveAIEnabled(Settings{}, config.Config{SAAS: true, AIEnabled: true}) {
		t.Fatal("SaaS unset community defaults AI on")
	}
}

func TestJoinPolicy(t *testing.T) {
	if got := JoinPolicy(Settings{JoinPolicy: "open"}, config.Config{SAAS: true}); got != "open" {
		t.Fatalf("SaaS override = %q, want open", got)
	}
	if got := JoinPolicy(Settings{JoinPolicy: "open"}, config.Config{SAAS: false}); got != "request" {
		t.Fatalf("self-host ignores override, default request, got %q", got)
	}
	if got := JoinPolicy(Settings{}, config.Config{SAAS: true}); got != "request" {
		t.Fatalf("SaaS unset community defaults request, got %q", got)
	}
}

func TestResolveStorage_OwnBucketOptOut(t *testing.T) {
	cfg := config.Config{SAAS: true, StorageBackend: "s3", S3Bucket: "platform", S3Region: "us-east-1"}
	// Community migrated to its own bucket.
	s := Settings{CommunityID: "c1", StorageBackend: "s3", S3Bucket: "tenant-private", S3AccessKey: "ak", S3SecretKey: "sk"}
	got := ResolveStorage(s, cfg)
	if !got.OwnBucket || got.S3Bucket != "tenant-private" {
		t.Fatalf("own-bucket opt-out must win, got %+v", got)
	}
	// A community that has NOT opted out uses the platform store.
	plat := ResolveStorage(Settings{CommunityID: "c2"}, cfg)
	if plat.OwnBucket || plat.S3Bucket != "platform" {
		t.Fatalf("default must be platform bucket, got %+v", plat)
	}
}
