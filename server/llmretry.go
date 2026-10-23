package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 再試行方針のサーバー側解析。docs/LLM重试设计.md を参照。五層のうち：
//   - 接続 / 空応答 / 同一 provider の安全区間は「エンドポイント単位」で、各 LLM 設定が
//     全体の既定を上書き可能（profile の空欄は全体を継承し、全体も未設定なら組み込み既定を使用）。
//   - 遮断 / インテント再実行はプロセス単位で、全体に一つだけ。
//
// 全体方針は DB の settings の一行を毎回読みます。呼び出しは低頻度（provider 構築、work 終了処理、
// 設定保存）で、追加キャッシュは不要です。遮断の引数は例外で、失敗経路で毎回読むため、
// applyRetryPolicy が Registry へ渡して保持させます。

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 回数はここでは「0=既定 / 負=無効」のままにします。SDK の MaxRetries /
		// EmptyResponseRetries と同じ構造なので、その解析に任せます。
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 遮断（順次切り替えの冷却）の既定は llmpool 組み込みと同じで、「ユーザーが設定した場合」だけ上書きします。
// インテント再実行の既定は engine.go の modelErrorRetries / modelErrorRetryBackoff を参照。

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob —— 「空応答の
// 再試行回数」。同じことへの異なる二手段です。SDK 層は「内容ブロック皆無」を対象とし、
// 同じ要求をそのまま再送します。ここは「思考のみで本文もツールもない」場合に指示を追加し、
// 既存の思考で続行させます（同じコンテキストに起因する空回りでは、同じ再送は無意味）。空判定が
// 異なるのは、SDK が yield したイベントの有無で判定し、思考差分自体もイベントだからです。ただしユーザーが
// 「空応答を何回再試行」と指定する意図は「実質的な内容がなければ再試行」なので、両層で回数を共有して
// その理解に合わせます。
//
// profile の上書きではなく全体方針を読みます。run の途中でフェイルオーバーにより profile が変わっても、
// インテント全体の総量上限はエンドポイントに追従させません。意味は SDK の emptyRetries() と同じ：
// 0 = 既定 defaultEmptyTurnNudges、-1（負）= 空回り続行を無効化、>0 = 指定値。
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
