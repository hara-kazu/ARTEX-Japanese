package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// ページのワンクリック更新の HTTP 層。実際のダウンロード/検証/入替ロジックはすべて selfupdate パッケージにあり、
// ここでは認証境界、並行排他、進捗配信、そして"終了すべき"と main に伝えることのみ担当する。
//
// 再起動はこのプロセスでは行わず、新版をステージング後に selfupdate.ExitRestart で終了し、
// 監視スクリプト（start.sh / start.bat、Docker では ENTRYPOINT）が再起動する。

// restartCh はアップグレード準備完了またはロールバック完了後に閉じ、main は受信後 ExitRestart で終了する。
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested は"終了して監視プロセスに再起動させてほしい"時に閉じる channel を返す。
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState は今回の起動時の selfupdate.Bootstrap の結論（アップグレード成功 / ロールバック直後 /
// ステージングファイルの破棄）で、main が注入し、/api/update/check が前回のアップグレード結果をフロントエンドへ正確に伝えるために使う。
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState は main が起動時に1回呼ぶ。
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache は GitHub の最新バージョン検索結果をキャッシュする。
//
// 上部バーの"新しいバージョンあり"の表示はページ全体の読み込みごとに1回照会するが、未認証の GitHub API は
// IP ごとに1時間 60 回なので、キャッシュがないと複数タブや数回のページ更新で割当枠を使い切り、
// 実際に更新したい際に照会できなくなる。ユーザーが明示的に"更新を確認"を押した場合は force でキャッシュを迂回できる。
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch はデータ取得関数で、テスト専用の注入箇所；nil なら実際の GitHub 照会を使用。
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 失敗結果も短時間キャッシュし、GitHub に到達できないときページ読込ごとにタイムアウトを待つことを防ぐ；
	// ただし TTL は短くし、ネットワーク復旧後に速やかに自然回復させる。
	releaseErrTTL = 2 * time.Minute
	// 照会用のタイムアウト。NewClient の 30 分タイムアウトはパッケージ全体のダウンロード用で、バージョン照会ではそこまで待てない。
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get は最新の Release を返し、キャッシュヒット時はネットワークにアクセスしない。
//
// 取得中はロックを保持：並行リクエストは個別に GitHub を呼ばず、同じ照会の結果を待つ
// （ページ読込直後に複数タブが同時照会すると、最もレート制限に達しやすい）。
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// リクエストのキャンセル（ユーザーがタブを閉じた）は GitHub の問題を意味せず、キャッシュに書き込まないこと。
	// そうしないと次の訪問者が理由不明の"キャンセル済み"エラーを受け取る。
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress はフロントエンドへ送る進捗1件。
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // ダウンロード段階のみ意味があり、他は -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub はアップグレード1回の進捗を保持し、SSE 購読者へ配信する。
//
// running は排他も兼ね、アップグレード中の再度の POST /api/update/apply は即 409 とし、
// 2つの goroutine が同じ artex.new へ同時に書き込むことを防ぐ。
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin はアップグレード権を取得し、進行中なら false を返す。
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "準備中…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish はアップグレード1回を終了する。err が nil ならステージング成功で、再起動待ちを示す。
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "更新に失敗しました", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "新しいバージョンの準備ができました。再起動しています…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout は h.mu を保持して呼ぶこと。購読者の channel はバッファ付きで、満杯なら破棄する——
// 進捗は破棄可能な一時情報なので、停止した SSE 接続1つにアップグレード自体を阻止させない。
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck は GitHub の最新正式版を照会し、現在のバージョンと比較する。
//
// フロントエンドも api.github.com に直接接続する（GitHub の CORS は *）が、**この API を正とする**：
// ダウンロードはバックエンドで行うため、バックエンドが GitHub にアクセスできて初めて更新可能となる。ブラウザは接続できてもサーバーはできない
// 場合はよくあり（サーバーが内部ネットワーク、またはプロキシがブラウザのみ設定）、その状態の更新は必ず失敗するので、
// 確認段階で正確にエラーを報告する方がよい。
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// 上部バーの表示はキャッシュ使用（デフォルト）；ユーザーが"更新を確認"を押すと force=1 で強制的に取得元へ照会する。
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 開発ビルド（dev / git describe に接尾辞あり）は比較可能なバージョン番号がなく、許可すると
		// ローカルでデバッグ中のバイナリを正式版で上書きしてしまうため、更新を許可しない。
		out["reason"] = fmt.Sprintf("現在のバージョン %q は正式リリースではないため、ワンクリック更新は無効です", current)
	}
	writeJSON(w, 200, out)
}

// updateApply は新版をダウンロードしてステージングし、完了後にプロセスを終了して監視スクリプトに再起動を委ねる。
//
// 即座に 202 を返し、実作業はバックグラウンドの goroutine で実行：パッケージ全体のダウンロードには数分かかることがあり、
// リクエストに紐付けるとリバースプロキシのタイムアウトで切断される。進捗は /api/update/stream を使用。
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// キャッシュを使用し、ユーザーが画面で確認して承認したバージョンそのものを導入する。
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("現在のバージョン %q は正式リリースではないため、ワンクリック更新は無効です", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("最新バージョン %s です", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "更新は既に実行中です")
		return
	}

	go func() {
		// リクエストの ctx ではなく、意図的に s.ctx を使用：HTTP 応答を返すとリクエストが終了し、
		// それに紐付けたダウンロードは即キャンセルされる。
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 更新に失敗しました：%v", err)
			return
		}
		log.Printf("[update] %s → %s を一時保存しました。入れ替えを完了するため、まもなく終了します", current, rel.TagName)
		// 最終進捗をフロントエンドへ送る時間を少し残してから、終了を発火する。
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback は前バージョン（入替前にバックアップした artex.old）へ能動的に戻す。
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "更新中のため、ロールバックできません")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 前のバージョンに手動でロールバックしました。切り替えを完了するため、まもなく終了します")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream は SSE で更新進捗を配信する。
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 先に現在の状態を1件補い、ページ更新後すぐに進行中のアップグレードが見えるようにする。
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
