// Package moderation runs an automated safety classifier (Llama Guard or
// ShieldGemma, via Ollama) over user chat messages and records a
// privacy-preserving AUDIT of any policy hit. The model is configurable
// (MODERATION_MODEL): Llama Guard is English-centric, while ShieldGemma is
// Gemma-2 based and handles many languages incl. Lithuanian — parseVerdict
// understands both output dialects (safe/unsafe+codes and Yes/No).
// It exists to make abuse detectable WITHOUT reading tenant content:
// it stores only a reference (community, message, author) plus the policy
// CATEGORY codes and the model — never the message body. That is what keeps the
// SaaS privacy wall intact (the platform operator cannot read a community's
// content — see auth.Identity.GodMode); abuse surfaces to the super-admin as
// counts + categories, which the Red flags panel aggregates.
//
// The classifier reuses the agent package's Ollama client (one non-streamed
// turn). Classification is fire-and-forget on a detached context: it is a model
// call and must never block or fail the chat send. Every classification is
// metered into ai_usage_events (feature "moderation") so the operator can see
// the compute it costs.
package moderation

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
)

// Feature is the ai_usage_events feature tag for metered classifications.
const Feature = "moderation"

// maxClassifyChars caps the text sent to the classifier. Llama Guard only needs
// the message to judge it; an abusive payload is abusive in its first few
// thousand chars, and the cap bounds prompt cost on a small local model.
const maxClassifyChars = 4000

// Verdict is the parsed outcome of one classification. Categories holds the
// Llama Guard hazard codes (e.g. "S3", "S12") when Flagged; it is empty for
// safe content. TokensIn/TokensOut are the provider-reported counts for metering.
type Verdict struct {
	Flagged    bool
	Categories []string
	TokensIn   int
	TokensOut  int
	// Raw is the model's unparsed reply, kept for debugging (the CLI `moderate`
	// command prints it). Audit ignores it.
	Raw string
}

// Auditor classifies messages and records flags. The zero value is not usable;
// build with NewAuditor. All exported methods are safe on a nil receiver, so a
// disabled feature (nil Auditor) is a no-op everywhere.
type Auditor struct {
	baseURL string
	model   string
	timeout time.Duration
	repo    *Repo
	usage   *aiusage.Recorder
	log     *slog.Logger
}

// NewAuditor builds an Auditor. timeout bounds a single classification so a slow
// or unreachable Ollama can't pile up goroutines. usage may be nil (metering
// off); repo must be set to persist flags.
func NewAuditor(baseURL, model string, timeout time.Duration, repo *Repo, usage *aiusage.Recorder, log *slog.Logger) *Auditor {
	return &Auditor{
		baseURL: strings.TrimSpace(baseURL),
		model:   strings.TrimSpace(model),
		timeout: timeout,
		repo:    repo,
		usage:   usage,
		log:     log,
	}
}

// Audit classifies one user message in the background and, on a policy hit,
// records a metadata-only flag. It returns immediately — classification runs on
// a detached, timeout-bounded context so it never blocks or fails the chat send
// (mirrors the fire-and-forget push/relay paths in chat.PostSend). Metering is
// recorded for every classification (the compute is spent regardless of the
// verdict); a flag row is written only when the content is flagged.
func (a *Auditor) Audit(communityID, channelID, messageID, authorID, body string) {
	if a == nil || a.model == "" {
		return
	}
	if strings.TrimSpace(body) == "" || communityID == "" || messageID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
		defer cancel()

		v, err := a.Classify(ctx, body)
		if err != nil {
			if a.log != nil {
				a.log.Warn("moderation: classify", "err", err, "community", communityID, "message", messageID)
			}
			return
		}
		// Meter the compute on every classification (safe or not) so the cost is
		// visible. Llama Guard reports real token counts via Ollama.
		a.usage.Record(ctx, aiusage.Event{
			CommunityID: communityID,
			Feature:     Feature,
			UserID:      authorID,
			Model:       a.model,
			TokensIn:    v.TokensIn,
			TokensOut:   v.TokensOut,
			Estimated:   v.TokensIn == 0 && v.TokensOut == 0,
		})
		// Log every verdict so operators can confirm the classifier is running
		// and see what it decided (flagged at Info, clean at Debug — set
		// LOG_LEVEL=debug to see the clean ones too).
		if a.log != nil {
			if v.Flagged {
				a.log.Info("moderation: flagged", "community", communityID, "message", messageID, "author", authorID, "categories", v.Categories, "model", a.model)
			} else {
				a.log.Debug("moderation: clean", "community", communityID, "message", messageID, "model", a.model)
			}
		}
		if !v.Flagged || a.repo == nil {
			return
		}
		if err := a.repo.Insert(ctx, Flag{
			CommunityID: communityID,
			MessageID:   messageID,
			ChannelID:   channelID,
			AuthorID:    authorID,
			Categories:  strings.Join(v.Categories, ","),
			Model:       a.model,
		}); err != nil && a.log != nil {
			a.log.Warn("moderation: insert flag", "err", err, "community", communityID, "message", messageID)
		}
	}()
}

// Classify runs one non-streamed classifier turn against the configured Ollama
// model and parses the verdict. The text is the LAST (and only) user turn; the
// model's own chat template wraps it in its safety-policy prompt (Llama Guard or
// ShieldGemma), so no system prompt is needed here. Exported so it is
// unit-testable against a live model; the parser is tested purely.
func (a *Auditor) Classify(ctx context.Context, text string) (Verdict, error) {
	text = strings.TrimSpace(text)
	if len(text) > maxClassifyChars {
		text = text[:maxClassifyChars]
	}
	o := agent.NewOllama(a.baseURL)
	// Temperature 0 = greedy decoding. A classifier MUST be deterministic: with
	// Ollama's default sampling the same message flips between verdicts run-to-run
	// (observed: "how to get Drugs?" returning "Yes" then blank then "Yes"). A
	// blank reply fail-opens (a missed violation), so non-determinism here is a
	// correctness bug, not just noise. We send the RAW text — no lowercasing
	// (casing was a red herring; the variance was sampling) and no hidden/bidi
	// stripping, since evasion via those chars is itself a signal to classify.
	o.Options = map[string]any{"temperature": 0}
	msgs := []agent.ChatMessage{{Role: "user", Content: text}}
	var b strings.Builder
	res, err := o.Stream(ctx, a.model, msgs, nil, func(d string) error {
		b.WriteString(d)
		return nil
	})
	if err != nil {
		return Verdict{}, err
	}
	v := parseVerdict(b.String())
	v.Raw = strings.TrimSpace(b.String())
	if res != nil {
		v.TokensIn = res.Usage.PromptTokens
		v.TokensOut = res.Usage.CompletionTokens
	}
	return v, nil
}

// guardCodeRE matches a Llama Guard hazard code (S1..S14).
var guardCodeRE = regexp.MustCompile(`(?i)\bS([0-9]{1,2})\b`)

// firstWordRE captures the verdict word at the START of a reply (after optional
// leading whitespace only). Anchored on purpose: both supported models LEAD
// with their verdict, so a letter-run found mid-reply ("0.97 unsafe") is not a
// verdict and must fall through to fail-open, not flag.
var firstWordRE = regexp.MustCompile(`^\s*([A-Za-z]+)`)

// parseVerdict reads a safety classifier reply, model-agnostically. Two output
// dialects are supported, distinguished by the leading verdict word:
//
//   - Llama Guard: "safe" / "unsafe" (and, when unsafe, category codes on a
//     following line). English-centric — weak on other languages.
//   - ShieldGemma: "Yes" (violates policy) / "No" (safe). Gemma-2 based, so it
//     handles many languages incl. Lithuanian — it carries no S-codes, so a
//     ShieldGemma flag records no categories.
//
// A reply that doesn't lead with a recognised verdict is treated as SAFE
// (fail-open: the audit must not invent flags from a garbled reply).
func parseVerdict(s string) Verdict {
	switch firstWord(s) {
	case "unsafe": // Llama Guard violation — pick out the S-codes
		return Verdict{Flagged: true, Categories: parseCodes(s)}
	case "yes": // ShieldGemma violation — no category taxonomy
		return Verdict{Flagged: true}
	case "safe", "no": // Llama Guard safe / ShieldGemma safe
		return Verdict{Flagged: false}
	default:
		return Verdict{Flagged: false}
	}
}

// firstWord returns the leading verdict word of s, lowercased ("" if the reply
// doesn't begin with a letter run). Lowercasing the model OUTPUT is what lets a
// ShieldGemma "Yes" or a Llama Guard "UNSAFE" match regardless of casing.
func firstWord(s string) string {
	m := firstWordRE.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// parseCodes extracts the distinct Llama Guard hazard codes from a reply,
// upper-cased, in first-seen order.
func parseCodes(s string) []string {
	seen := make(map[string]struct{})
	var cats []string
	for _, m := range guardCodeRE.FindAllStringSubmatch(s, -1) {
		key := strings.ToUpper("S" + m[1])
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		cats = append(cats, key)
	}
	return cats
}

// categoryLabels maps Llama Guard 3 hazard codes to human labels (MLCommons
// taxonomy). Used by UI to render the audit; unknown codes fall back to the raw
// code.
var categoryLabels = map[string]string{
	"S1":  "Violent crimes",
	"S2":  "Non-violent crimes",
	"S3":  "Sex-related crimes",
	"S4":  "Child sexual exploitation",
	"S5":  "Defamation",
	"S6":  "Specialized advice",
	"S7":  "Privacy",
	"S8":  "Intellectual property",
	"S9":  "Indiscriminate weapons",
	"S10": "Hate",
	"S11": "Suicide & self-harm",
	"S12": "Sexual content",
	"S13": "Elections",
	"S14": "Code interpreter abuse",
}

// CategoryLabel returns the human label for a Llama Guard code, or the code
// itself if unknown.
func CategoryLabel(code string) string {
	if l, ok := categoryLabels[strings.ToUpper(strings.TrimSpace(code))]; ok {
		return l
	}
	return code
}
