package config

import "testing"

func TestLoad_SaaSReshapesGlobals(t *testing.T) {
	t.Setenv("SAAS", "true")
	t.Setenv("MAILBOX_ENABLED", "true")    // SaaS must force this off
	t.Setenv("OPEN_REGISTRATION", "false") // SaaS must force this on
	t.Setenv("ENV", "dev")                 // dev: no SECRETS_KEY required
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MailboxEnabled {
		t.Error("SaaS must disable IMAP mailbox")
	}
	if !cfg.OpenRegistration {
		t.Error("SaaS must force open registration")
	}
	if got := cfg.EffectiveStorageBackend(); got != "s3" {
		t.Errorf("SaaS default storage = %q, want s3", got)
	}
}

func TestLoad_ProdSaaSRequiresSecretsKey(t *testing.T) {
	t.Setenv("SAAS", "true")
	t.Setenv("ENV", "prod")
	t.Setenv("SESSION_KEY", "a-real-prod-session-key-32-bytes!")
	t.Setenv("UPLOADS_SIGN_KEY", "a-real-prod-uploads-sign-key-ok!")
	t.Setenv("SECRETS_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("prod + SaaS without SECRETS_KEY must be rejected")
	}
}

func TestEffectiveStorageBackend(t *testing.T) {
	if (Config{SAAS: false}).EffectiveStorageBackend() != "disk" {
		t.Error("self-hosted default must be disk")
	}
	if (Config{StorageBackend: "s3", SAAS: false}).EffectiveStorageBackend() != "s3" {
		t.Error("explicit STORAGE_BACKEND must win")
	}
}
