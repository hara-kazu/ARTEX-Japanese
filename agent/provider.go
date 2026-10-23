// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType は思考の「有効/無効」フィールド(thinking.type)を独立制御：
	//   "" = 送信しない(デフォルト、非対応モデルと互換); "disabled" = 明示的に無効;
	//   "enabled" = 有効. ReasoningEffort とは完全に独立——thinking フィールドがなく、
	//   強度パラメーターだけで思考を有効にする API もあるため、各々単独で設定可能.
	ThinkingType string
	// ReasoningEffort は思考の「強度」フィールドを独立制御：
	//   "" = 送信しない(デフォルト); "low"/"medium"/"high"/"xhigh"/"max" = 対応する強度.
	//   OpenAI は最上位の reasoning_effort に対応;Anthropic は output_config.effort に対応.
	ReasoningEffort string
	// Stream はこの profile がストリーミング(SSE) API を使うか制御する。true(デフォルト)= ストリーミング;false = 真の
	// 非ストリーミング(stream:false を送り、完全な JSON を一括取得し、Provider.Complete を使用)。非ストリーミングは
	// 一部ゲートウェイの不完全な SSE 実装(空フレーム、思考フィールドのフレーム欠落)を回避できるが、実行中のリアルタイム進捗/リアルタイム
	// token 集計を失う。agentcore.Options.NonStreaming = !Stream に対応。
	Stream bool
	// MaxTokens は1回の応答の出力上限(token)。0 = このフィールドを送らず、サーバー側のデフォルト値に委ねる
	// (従来の挙動)。ContextWindowK とは異なり、後者はモデル全体の容量で、ローカルの圧縮閾値計算のみに使用し、
	// リクエストには含まれない;この値は各リクエストで送信する。agentcore.Options.MaxTokens に対応。
	MaxTokens int
	// MaxTokensField は MaxTokens に使用するリクエストフィールド名を選び、format=openai のみ有効:
	//   "" = max_tokens(デフォルト); "max_completion_tokens" = 新フィールド。
	// OpenAI の推論モデル(o 系列/GPT-5)は後者のみ認識し、max_tokens を受信すると直接
	// unsupported_parameter になる;互換ゲートウェイの大半は前者のみ認識するため、自動推論せず、端点ごとにユーザーが選択する。
	MaxTokensField string
	// SessionHeaderKey が非空なら、各 LLM リクエストにカスタム HTTP ヘッダーを付け、ヘッダー名はこの値、
	// ヘッダー値は【現在のセッションの session id】(chat セッション=conv-<id>、worker=exp<x>-worker-i<intent>
	// など。WorkerSessionID を参照)。session-id ヘッダーでプロンプトキャッシュ/スティッキールーティングを行う一部ゲートウェイ用。
	// 空 = 送信しない。値は transcript.WithSessionID がリクエスト context に付け、RoundTripper が
	// 読み取って設定するため、同じ共有 provider でもセッションごとに異なるヘッダー値を送れる。
	SessionHeaderKey string
	// Retry はこの設定の解決済み再試行パラメーター(profile の上書き → グローバルポリシー → 組み込みデフォルト、
	// server 側で解決)。3層の意味は RetryConfig を参照;ゼロ値 = 組み込みデフォルトを完全に継続。
	Retry RetryConfig
}

// RetryConfig は LLM 設定に付随する再試行パラメーター。各層の「回数」は共通の意味:
// 0 = 組み込みのデフォルト回数;負数 = この層の再試行を無効;>0 = この値を使用。各層の「間隔」:
// 0 = この層の元の指数バックオフ;>0 = この固定間隔を使用。
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval:SDK 接続再試行(接続リセット/タイムアウト/429/5xx、ストリーム開始前)で、
	// llm.Config.MaxRetries / RetryInterval に直接対応。デフォルト 3 回、0.5s から指数増加(上限 8s)。
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval:SDK 空レスポンス再試行(完了したが content block なし、openai
	// 形式のみ)で、llm.Config.EmptyResponseRetries / EmptyResponseInterval に対応。
	// デフォルト 2 回、同じ指数段階。
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval:同じ provider の安全ウィンドウ内の再試行——本プロジェクトが SDK の上に補う
	// 1層で、「まだ呼び出し元へ出力を一切渡していない」場合のみ、切断/過負荷/ストリーム内の 429 を再実行する。SDK からは見えず、
	// server/task_llm.go が使用する。デフォルト 2 回、0.5s から指数増加(上限 4s)。
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// デフォルトはストリーミング;ARTEX_LLM_STREAM=false/0/off で明示的に無効にし、非ストリーミングを使用。
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // デフォルトはストリーミング;呼び出し元が profile に従って上書き
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 思考の有効/無効と強度の2フィールドは各々そのまま渡す(空 = そのフィールドは送らない)。両者は独立:
	// thinking.type のみ、effort のみ、両方、または両方なしで送信可能。
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 出力上限のフィールド名選択(空 = max_tokens を使用)。上限の「値」はここにはなく、各ラウンドの
	// agentcore.Options.MaxTokens に従い、provider はどのキーに格納するかだけを決める。
	lc.MaxTokensField = c.MaxTokensField
	// 再試行パラメーターは SDK と同じ意味(回数 0=デフォルト/負=無効、間隔 0=指数バックオフ/>0=固定)で、そのまま渡す。
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 空=直接接続、HTTP_PROXY/HTTPS_PROXY 環境変数へフォールバックしない
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "クリックしてテスト" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP リクエストを送信していません（設定の解析または接続の確立時に失敗）",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 試行 %d/%d HTTP %d\n応答本文: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "（空）"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…（切り詰め済み、合計%dバイト）", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 元の wire メッセージを取得:接続テストで最も必要なのは、ゲートウェイが実際に何を返したか(状態コード+レスポンス本文)で、
	// norma がレスポンスを StreamEvent に復号するとこれらは失われる。quotaAwareTransport は
	// context 内でこの Capture を見つけ、各 HTTP 試行の状態コードと body を設定する。
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 接続テストは単発経路で、agentcore のセッションループを経由しないため、誰も context に
	// session id を付けない。SessionHeaderKey を設定した端点(opencode zen は
	// x-opencode-session ヘッダーを必須とし、欠落すると即 400 MissingSessionID など)では、"対話は正常なのに、
	// クリックしてテストすると 400"という差が生じる。ここで使い捨てのランダムな session id を補い、テストも実際の対話と同じ
	// ヘッダー送信ロジックを使わせる;SessionHeaderKey 未設定の端点は読まないため、副作用なし。
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens は十分に与える：推論モデル(deepseek-v4-pro など)は回答前に長い
	// 思考を出力する(実測では "ping" の一言でも ~2900 token を消費)。32 だけ与えると、モデルが"思考段階"のままで
	// 出力上限(finish=length)に達して切り詰められ、接続テストは疎通成功(err=nil)でも表示が
	// "中断済み/length/resume" となり混乱する。十分な予算で OK を最後まで出力させる(finish=stop)。
	// EscalateMaxTokens は false を維持:切り詰めによる上限引き上げ再試行をせず、resume ループによる無駄な消費を防ぐ。
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"接続テストです。OK の二文字だけを直接出力し、思考、説明、その他の内容は出力しないでください。"},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // この profile の実際の送受信モードで接続テストを行う
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil だけでは不足：疎通してもモデルが一文字も出力しない場合が実際にある(思考が予算を消費し尽くす、
	// 本文が安全ポリシーで失われる、互換層が content を落とす)。この設定はセッションで"応答しない"のに、
	// テストでは成功となる——この差を解消する。可視の本文がなければ一律失敗と判定する。
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("モデルの応答内容がありません（リクエストは成功しましたが、テキストが返されませんでした）")
	}
	return lat, reply, nil
}
