package llm

import (
	"net/url"
	"regexp"
	"strings"
)

// Provider is a hosted model service with an OpenAI-compatible chat API.
// Thicket calls it like any other OpenAI-compatible server, with the key
// stored as the model API key; what the catalog adds is the address, where
// to get a key, how to recognise one, and which of its models to pick.
//
// OpenRouter is not in the catalog: it has its own key slot and
// extensions (cost reporting, PDF parsing, web search).
type Provider struct {
	ID   string // short name used in settings and the environment (THICKET_PROVIDER)
	Name string // for people
	// BaseURL is the OpenAI-compatible API, ending before /chat/completions.
	BaseURL string
	// Hosts are the API hostnames that identify the provider from a
	// model_url (the BaseURL's own host is always included).
	Hosts []string
	// PathPrefix, if set, must start the model_url path too: two plans
	// of one service can share a host (a coding plan's endpoint).
	PathPrefix string
	KeysURL    string // where to make a key
	// KeyRe matches this provider's keys and no other's. Nil when its
	// keys look like other providers' (plain sk-...): then the owner names
	// the provider along with the key.
	KeyRe *regexp.Regexp
	// Names are words that mean this provider in a chat message.
	Names []string
	// Subscription: a flat monthly plan, so per-call spend is $0.
	Subscription bool
	// Prefer and PreferCheap are substrings, best first, used to pick
	// the main and the cheap model from what the provider lists. Models
	// change often, so these are matched against the live list rather than
	// trusted as ids; with no match, the first chat model is used.
	Prefer, PreferCheap []string
	// Note is shown when connecting (limits, quirks).
	Note string
	// Headers go with every request besides the bearer key (Anthropic's
	// model list wants its own key header and an API version).
	Headers func(key string) map[string]string
}

// Providers is the catalog, in the order they are offered.
var Providers = []Provider{
	{
		ID: "opencode-zen", Name: "OpenCode Zen", BaseURL: "https://opencode.ai/zen/v1",
		PathPrefix: "/zen/v1", KeysURL: "https://opencode.ai/auth",
		Names:  []string{"opencode zen", "zen"},
		Prefer: []string{"kimi-k", "glm-", "qwen3-coder", "grok"}, PreferCheap: []string{"flash", "mini", "nano", "air"},
		Note: "Pay-as-you-go. Some Zen models only speak Anthropic's or OpenAI's newer APIs; Thicket uses the ones on /chat/completions.",
	},
	{
		ID: "opencode-go", Name: "OpenCode Go (subscription)", BaseURL: "https://opencode.ai/zen/go/v1",
		PathPrefix: "/zen/go/v1", KeysURL: "https://opencode.ai/go",
		Names: []string{"opencode go", "opencode-go", "opencode"}, Subscription: true,
		Prefer: []string{"kimi-k", "glm-", "grok"}, PreferCheap: []string{"glm-", "kimi-k"},
		Note: "Flat monthly plan with usage limits. Only GLM, Kimi and Grok models on Go use /chat/completions, so pick one of those.",
	},
	{
		ID: "nous", Name: "Nous Portal (Nous Research)", BaseURL: "https://inference-api.nousresearch.com/v1",
		Hosts: []string{"inference.nousresearch.com"}, KeysURL: "https://portal.nousresearch.com",
		Names:  []string{"nous portal", "nous research", "nousresearch", "nous", "hermes"},
		Prefer: []string{"hermes-4-405b", "hermes-4-70b", "hermes"}, PreferCheap: []string{"hermes-4.3", "36b", "70b"},
		Note: "Hermes models are strong conversationalists; for heavy tool use a bigger one (405B) works best.",
	},
	{
		ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", KeysURL: "https://platform.openai.com/api-keys",
		Names:  []string{"openai", "chatgpt"},
		Prefer: []string{"gpt-5", "gpt-4.1", "gpt-4o"}, PreferCheap: []string{"mini", "nano"},
	},
	{
		ID: "anthropic", Name: "Anthropic", BaseURL: "https://api.anthropic.com/v1", KeysURL: "https://console.anthropic.com/settings/keys",
		KeyRe: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`), Names: []string{"anthropic", "claude"},
		Prefer: []string{"sonnet", "opus"}, PreferCheap: []string{"haiku"},
		Note: "Uses Anthropic's OpenAI-compatible endpoint.",
		Headers: func(key string) map[string]string {
			return map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
		},
	},
	{
		ID: "gemini", Name: "Google Gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		PathPrefix: "/v1beta/openai", KeysURL: "https://aistudio.google.com/apikey",
		KeyRe: regexp.MustCompile(`\bAIza[A-Za-z0-9_\-]{35}\b`), Names: []string{"gemini", "google ai studio", "google"},
		Prefer: []string{"gemini-3-pro", "gemini-2.5-pro", "pro"}, PreferCheap: []string{"flash-lite", "flash"},
	},
	{
		ID: "xai", Name: "xAI (Grok)", BaseURL: "https://api.x.ai/v1", KeysURL: "https://console.x.ai",
		KeyRe: regexp.MustCompile(`\bxai-[A-Za-z0-9]{20,}`), Names: []string{"xai", "x.ai", "grok"},
		Prefer: []string{"grok-4", "grok-3"}, PreferCheap: []string{"fast", "mini"},
	},
	{
		ID: "mistral", Name: "Mistral", BaseURL: "https://api.mistral.ai/v1", KeysURL: "https://console.mistral.ai/api-keys",
		Names:  []string{"mistral", "la plateforme"},
		Prefer: []string{"mistral-large", "mistral-medium"}, PreferCheap: []string{"mistral-small", "ministral"},
	},
	{
		ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", KeysURL: "https://platform.deepseek.com/api_keys",
		Names:  []string{"deepseek"},
		Prefer: []string{"deepseek-chat"}, PreferCheap: []string{"deepseek-chat"},
	},
	{
		ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com/openai/v1", KeysURL: "https://console.groq.com/keys",
		KeyRe: regexp.MustCompile(`\bgsk_[A-Za-z0-9]{20,}`), Names: []string{"groq"},
		Prefer: []string{"kimi-k2", "gpt-oss-120b", "llama-3.3-70b"}, PreferCheap: []string{"gpt-oss-20b", "llama-3.1-8b", "instant"},
		Note: "Very fast. The free tier has low rate limits.",
	},
	{
		ID: "cerebras", Name: "Cerebras", BaseURL: "https://api.cerebras.ai/v1", KeysURL: "https://cloud.cerebras.ai",
		KeyRe: regexp.MustCompile(`\bcsk-[A-Za-z0-9]{20,}`), Names: []string{"cerebras"},
		Prefer: []string{"qwen-3-235b", "gpt-oss-120b", "llama-3.3-70b"}, PreferCheap: []string{"llama3.1-8b", "8b"},
	},
	{
		ID: "together", Name: "Together AI", BaseURL: "https://api.together.xyz/v1", KeysURL: "https://api.together.ai/settings/api-keys",
		Names:  []string{"together ai", "together.ai", "togetherai"},
		Prefer: []string{"Kimi-K2", "Qwen3-235B", "Llama-3.3-70B"}, PreferCheap: []string{"Llama-3.1-8B", "8B"},
	},
	{
		ID: "fireworks", Name: "Fireworks AI", BaseURL: "https://api.fireworks.ai/inference/v1", KeysURL: "https://fireworks.ai/account/api-keys",
		KeyRe: regexp.MustCompile(`\bfw_[A-Za-z0-9]{20,}`), Names: []string{"fireworks"},
		Prefer: []string{"kimi-k2", "qwen3-235b", "llama-v3p3-70b"}, PreferCheap: []string{"llama-v3p1-8b", "8b"},
	},
	{
		ID: "deepinfra", Name: "DeepInfra", BaseURL: "https://api.deepinfra.com/v1/openai", KeysURL: "https://deepinfra.com/dash/api_keys",
		Names:  []string{"deepinfra"},
		Prefer: []string{"Kimi-K2", "Qwen3-235B", "Llama-3.3-70B"}, PreferCheap: []string{"Llama-3.1-8B", "8B"},
	},
	{
		ID: "huggingface", Name: "Hugging Face Inference Providers", BaseURL: "https://router.huggingface.co/v1", KeysURL: "https://huggingface.co/settings/tokens",
		KeyRe: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`), Names: []string{"hugging face", "huggingface", "hf"},
		Prefer: []string{"Kimi-K2", "Qwen3-235B", "Llama-3.3-70B"}, PreferCheap: []string{"Llama-3.1-8B", "8B"},
	},
	{
		ID: "nvidia", Name: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1", KeysURL: "https://build.nvidia.com",
		KeyRe: regexp.MustCompile(`\bnvapi-[A-Za-z0-9_\-]{20,}`), Names: []string{"nvidia nim", "nvidia"},
		Prefer: []string{"llama-3.3-70b", "nemotron"}, PreferCheap: []string{"8b"},
	},
	{
		ID: "moonshot", Name: "Moonshot AI (Kimi)", BaseURL: "https://api.moonshot.ai/v1", KeysURL: "https://platform.moonshot.ai/console/api-keys",
		Names:  []string{"moonshot", "kimi"},
		Prefer: []string{"kimi-k2", "kimi"}, PreferCheap: []string{"kimi-k2", "moonshot-v1-8k"},
	},
	{
		ID: "zai", Name: "Z.ai (GLM)", BaseURL: "https://api.z.ai/api/paas/v4",
		PathPrefix: "/api/paas", KeysURL: "https://z.ai/manage-apikey/apikey-list",
		Names:  []string{"z.ai api", "zai api", "zhipu"},
		Prefer: []string{"glm-4.6", "glm-4.5", "glm-"}, PreferCheap: []string{"air", "flash"},
	},
	{
		ID: "zai-coding", Name: "Z.ai GLM Coding Plan (subscription)", BaseURL: "https://api.z.ai/api/coding/paas/v4",
		PathPrefix: "/api/coding", KeysURL: "https://z.ai/subscribe",
		Names: []string{"glm coding plan", "coding plan", "z.ai", "zai", "glm"}, Subscription: true,
		Prefer: []string{"glm-4.6", "glm-4.5", "glm-"}, PreferCheap: []string{"air", "glm-"},
		Note: "Flat monthly plan. Coding Plan keys only work on this endpoint, not Z.ai's pay-as-you-go one.",
	},
	{
		ID: "chutes", Name: "Chutes (subscription)", BaseURL: "https://llm.chutes.ai/v1", KeysURL: "https://chutes.ai/app/api",
		KeyRe: regexp.MustCompile(`\bcpk_[A-Za-z0-9._\-]{20,}`), Names: []string{"chutes"}, Subscription: true,
		Prefer: []string{"Kimi-K2", "DeepSeek-V3", "Qwen3-235B", "GLM-4"}, PreferCheap: []string{"Qwen3-30B", "8B"},
		Note: "Daily request allowance on a monthly plan.",
	},
	{
		ID: "synthetic", Name: "Synthetic (subscription)", BaseURL: "https://api.synthetic.new/openai/v1", KeysURL: "https://synthetic.new/user-settings/api",
		Names: []string{"synthetic.new", "synthetic"}, Subscription: true,
		Prefer: []string{"Kimi-K2", "GLM-4", "DeepSeek-V3", "Qwen3-235B"}, PreferCheap: []string{"GLM-4", "Qwen3"},
		Note: "Flat monthly plan with a request allowance per few hours.",
	},
	{
		ID: "venice", Name: "Venice AI", BaseURL: "https://api.venice.ai/api/v1", KeysURL: "https://venice.ai/settings/api",
		Names:  []string{"venice"},
		Prefer: []string{"qwen3-235b", "llama-3.3-70b"}, PreferCheap: []string{"venice-small", "qwen3-4b", "small"},
	},
}

// ProviderByID finds a catalog provider by its ID or name, ignoring case.
func ProviderByID(id string) (Provider, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range Providers {
		if p.ID == id || strings.ToLower(p.Name) == id {
			return p, true
		}
	}
	return Provider{}, false
}

// ProviderFor finds the catalog provider a base URL belongs to.
func ProviderFor(baseURL string) (Provider, bool) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return Provider{}, false
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range Providers {
		pu, _ := url.Parse(p.BaseURL)
		match := host == pu.Hostname()
		for _, h := range p.Hosts {
			match = match || host == h
		}
		if match && (p.PathPrefix == "" || strings.HasPrefix(u.Path, p.PathPrefix)) {
			return p, true
		}
	}
	return Provider{}, false
}

// IsHosted reports whether a base URL is a hosted service (OpenRouter or a
// catalog provider) rather than a server the owner runs.
func IsHosted(baseURL string) bool {
	if IsOpenRouter(baseURL) {
		return true
	}
	_, ok := ProviderFor(baseURL)
	return ok
}

// ProviderByKey recognises a key whose shape only one provider uses, and
// returns the key itself.
func ProviderByKey(text string) (Provider, string, bool) {
	for _, p := range Providers {
		if p.KeyRe == nil {
			continue
		}
		if k := p.KeyRe.FindString(text); k != "" {
			return p, k, true
		}
	}
	return Provider{}, "", false
}

var genericKeyRe = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{20,}|[A-Za-z0-9_\-.]{32,})`)

// ProviderInText finds a provider named in a message ("deepseek sk-…",
// "use groq"). Longer names are tried first, so "opencode go" wins over
// "opencode" and "glm coding plan" over "glm".
func ProviderInText(text string) (Provider, bool) {
	l := " " + strings.ToLower(text) + " "
	best, bestLen := Provider{}, 0
	for _, p := range Providers {
		for _, n := range p.Names {
			if len(n) <= bestLen {
				continue
			}
			i := strings.Index(l, n)
			if i < 0 {
				continue
			}
			// whole words only: "hf" must not match inside "hfoo"
			before, after := l[i-1], l[i+len(n)]
			if isWordByte(before) || isWordByte(after) {
				continue
			}
			best, bestLen = p, len(n)
		}
	}
	return best, bestLen > 0
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

// KeyInText finds the key in a message that names its provider: the
// provider's own key shape if it has one, else the first long token
// that looks like a key.
func KeyInText(p Provider, text string) string {
	if p.KeyRe != nil {
		return p.KeyRe.FindString(text)
	}
	for _, m := range genericKeyRe.FindAllString(text, -1) {
		if strings.Contains(m, "://") || strings.Count(m, ".") > 2 {
			continue
		}
		return m
	}
	return ""
}

// PickModels chooses the main and cheap model from what a provider lists.
func (p Provider) PickModels(ids []string) (main, cheap string) {
	chat := ChatModels(ids)
	if len(chat) == 0 {
		return "", ""
	}
	pick := func(prefs []string) string {
		for _, want := range prefs {
			w := strings.ToLower(want)
			for _, id := range chat {
				if strings.Contains(strings.ToLower(id), w) {
					return id
				}
			}
		}
		return ""
	}
	main = pick(p.Prefer)
	if main == "" {
		main = chat[0]
	}
	cheap = pick(p.PreferCheap)
	if cheap == "" {
		cheap = main
	}
	return main, cheap
}
