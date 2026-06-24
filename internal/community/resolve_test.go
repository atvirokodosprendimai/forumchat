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

// platformCfg is a SaaS config with the operator's hosted AI fully configured.
func platformCfg() config.Config {
	return config.Config{
		SAAS: true, RAGEnabled: true, TranslateEnabled: true,
		PlatformAIRAGBaseURL: "http://platform:11434", PlatformAIRAGModel: "bge-m3", PlatformAIRAGDim: 1024,
		PlatformAIQdrantURL: "http://platform-qdrant:6333", PlatformAIQdrantAPIKey: "pkey",
		PlatformAITranslateBaseURL: "http://platform:11434", PlatformAITranslateModel: "gemma",
		PlatformAIAgentProvider: "ollama", PlatformAIAgentBaseURL: "http://platform:11434", PlatformAIAgentModel: "llama",
	}
}

// platformOn is a community opted into platform AI and authorized via grant.
func platformOn() Settings {
	return Settings{CommunityID: "c1", UsePlatformAI: ptrBool(true), PlatformAIGrantedFree: ptrBool(true)}
}

func TestPlatformAI_Authorization(t *testing.T) {
	cfg := config.Config{SAAS: true}
	// Self-hosted: never platform.
	if on, _ := PlatformAI(Settings{UsePlatformAI: ptrBool(true)}, config.Config{SAAS: false}); on {
		t.Fatal("self-host has no platform tier")
	}
	// Opted in but unauthorized.
	if on, auth := PlatformAI(Settings{UsePlatformAI: ptrBool(true)}, cfg); !on || auth {
		t.Fatalf("opted-in unauthorized: on=%v auth=%v, want true,false", on, auth)
	}
	// Granted free → authorized.
	if _, auth := PlatformAI(Settings{UsePlatformAI: ptrBool(true), PlatformAIGrantedFree: ptrBool(true)}, cfg); !auth {
		t.Fatal("granted-free must authorize")
	}
	// Active subscription → authorized.
	if _, auth := PlatformAI(Settings{UsePlatformAI: ptrBool(true), StripeSubscriptionStatus: "active"}, cfg); !auth {
		t.Fatal("active subscription must authorize")
	}
	// Canceled subscription, no grant → unauthorized.
	if _, auth := PlatformAI(Settings{UsePlatformAI: ptrBool(true), StripeSubscriptionStatus: "canceled"}, cfg); auth {
		t.Fatal("canceled subscription must not authorize")
	}
}

func TestResolveRAG_PlatformTier(t *testing.T) {
	cfg := platformCfg()
	got := ResolveRAG(platformOn(), cfg)
	if !got.Platform || !got.Enabled {
		t.Fatalf("authorized opt-in must use platform + be enabled, got %+v", got)
	}
	if got.EmbedBaseURL != "http://platform:11434" || got.EmbedModel != "bge-m3" || got.QdrantURL != "http://platform-qdrant:6333" {
		t.Fatalf("platform RAG must source PLATFORM_AI_*, got %+v", got)
	}
	if got.QdrantColl != "forumchat_c1" {
		t.Fatalf("per-community collection isolation must hold on platform, got %q", got.QdrantColl)
	}

	// Opted in but NOT authorized → BYO path (Platform false), not platform.
	unauth := Settings{CommunityID: "c1", UsePlatformAI: ptrBool(true)}
	if ResolveRAG(unauth, cfg).Platform {
		t.Fatal("unauthorized opt-in must fall through to BYO, not platform")
	}

	// Authorized but operator hasn't configured PLATFORM_AI_RAG_BASEURL → BYO.
	noPlat := cfg
	noPlat.PlatformAIRAGBaseURL = ""
	if ResolveRAG(platformOn(), noPlat).Platform {
		t.Fatal("unset platform endpoint must fall through to BYO")
	}

	// Kill-switch off disables even on the platform branch.
	killed := platformCfg()
	killed.RAGEnabled = false
	if ResolveRAG(platformOn(), killed).Enabled {
		t.Fatal("kill-switch off must disable platform RAG too")
	}
}

func TestResolveTranslate_PlatformTier(t *testing.T) {
	got := ResolveTranslate(platformOn(), platformCfg())
	if !got.Platform || !got.Enabled || got.BaseURL != "http://platform:11434" || got.Model != "gemma" {
		t.Fatalf("authorized opt-in must use platform translate, got %+v", got)
	}
}

func TestResolveAgent_PlatformTier(t *testing.T) {
	cfg := platformCfg()
	cfg.PlatformAIAgentVisionModel = "gemma4"

	// Text agent → text model.
	got := ResolveAgent(platformOn(), cfg, false)
	if !got.Platform || got.BaseURL != "http://platform:11434" || got.Model != "llama" || got.Provider != "ollama" {
		t.Fatalf("text agent must use platform text model, got %+v", got)
	}
	// Vision agent → vision model.
	if v := ResolveAgent(platformOn(), cfg, true); !v.Platform || v.Model != "gemma4" {
		t.Fatalf("vision agent must use platform vision model, got %+v", v)
	}
	// Vision agent but operator configured no vision model → stays BYO.
	noVis := platformCfg() // vision model empty
	if ResolveAgent(platformOn(), noVis, true).Platform {
		t.Fatal("vision agent with no platform vision model must stay BYO")
	}
	// Text agent still works when only the text model is set.
	if !ResolveAgent(platformOn(), noVis, false).Platform {
		t.Fatal("text agent must still platform-route when text model is set")
	}
	// Not opted in → agent runs on its own backend (Platform false).
	if ResolveAgent(Settings{CommunityID: "c1"}, cfg, false).Platform {
		t.Fatal("no opt-in must leave the agent on its BYO backend")
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
