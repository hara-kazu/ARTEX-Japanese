package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv はスモークテストで起動した子プロセスに Bootstrap を直接スキップさせる。
//
// 厳密には付けなくても問題はない：子プロセスの os.Executable() は artex.new で、導出される
// すべてのパスに .new 接頭辞が付き、実際の更新ファイルには触れない。ただし、この偶然に依存するのは脆弱すぎるため、
// 明示的な短絡で分かりやすくし、子プロセスによる無駄なディスク探索も1回省く。
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action は Bootstrap から main への指示。
type Action int

const (
	// Continue：通常どおり server を起動する。
	Continue Action = iota
	// Restart：直ちに ExitRestart で終了し、監視スクリプトに再起動させる。
	Restart
)

// State は今回起動時の更新状態を表し、/api/update/check がフロントエンドに
// 「前回の更新が成功したか、ロールバックされたか」を正確に伝えるために使う。
type State struct {
	Pending     bool   // 置換後、安定動作が未確認
	RolledBack  bool   // 今回の起動で自動ロールバックを実行済み
	FailedStage bool   // 一時保存ファイルが検証/スモークテストに失敗し、破棄済み
	Detail      string // ユーザー向けの1文の説明
}

// Bootstrap は main の最初に実行し、ポート待ち受けやデータベースを開く前に呼び出す必要がある。
//
// 3つの状況：
//
//	① 一時保存ファイル artex.new がある → 検証 + スモークテスト、成功なら置換して再起動を要求、失敗なら破棄して旧版を続行
//	② マーカーファイルのみ残る          → 置換直後を示し、試行を1回加算。連続失敗が規定回数に達したらロールバック
//	③ 何もない                          → 通常起動
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 起動時の更新処理をスキップします：%v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged は「一時保存ファイルがある」状況を処理する：検証成功なら置換し、失敗なら破棄する。
//
// ここは更新経路全体で実行ファイルを上書きする唯一の場所で、最後の関門でもある。スモークテストで
// ダウンロード破損、アーキテクチャの選択誤り、動的リンクの不足などを防ぐ。実行できないバイナリーを許可すると、
// 監視スクリプトが延々と再起動する一方、Go コードには実行機会がなく、自動ロールバックも不可能になる。
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 一時保存した新しいバージョンの検証に失敗したため破棄し、現在のバージョンを継続します：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "新しいバージョンの検証に失敗したため、破棄しました：" + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 実行ファイルの置き換えに失敗したため、現在のバージョンを継続します：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "実行ファイルの置き換えに失敗しました：" + err.Error()}
	}

	// 置換成功。マーカーを保持し、次回起動（新版が動く）で安定性を確認する。
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 更新マーカーの書き込みに失敗しました（自動ロールバックを利用できません）：%v", err)
	}
	log.Printf("[update] %s に置き換えました。再起動のため終了します（exit %d）", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback は「置換後の起動」を処理する：試行回数を加算し、上限超過なら旧版に戻す。
//
// カウントは Go コードの起動後にのみ増えるため、「実行できるが初期化でクラッシュする」
// （設定非互換、ポート使用中、DB マイグレーション失敗）などの障害をカバーする。「exec 自体が不可能」は置換前の
// スモークテストで防ぎ、両者を合わせて初めて完全となる。
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// ロールバック自体が失敗したら、無限再起動を避けるため再起動しない。マーカーを消去し、
			// 現状のままプロセスを起動する。起動できなくても、ユーザーは少なくともログで理由を確認できる。
			log.Printf("[update] 新しいバージョンの起動に %d 回連続で失敗し、ロールバックにも失敗しました：%v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "新しいバージョンの起動とロールバックに失敗しました：" + err.Error()}
		}
		log.Printf("[update] 新しいバージョンの起動に %d 回連続で失敗したため、%s に戻しました。再起動のため終了します（exit %d）",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("新しいバージョンの起動に失敗したため、%s に戻しました", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 更新マーカーの更新に失敗しました：%v", err)
	}
	log.Printf("[update] 新しいバージョンを起動しています（%d/%d 回目）。安定して動作した後に更新を確定します",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle は新版の安定動作を確認し、更新マーカーを消去する。
//
// main が HTTP の待ち受け開始後、遅延して呼び出す。この時間を生き延びて初めて成功とし、それ以外はマーカーを残し、
// 次回起動で試行回数の加算を続け、ロールバックを発生させる。
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 更新後の起動でなければ、処理は不要
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 更新マーカーの削除に失敗しました：%v", err)
		return
	}
	log.Printf("[update] 新しいバージョンが安定して動作したため、更新を完了しました（前のバージョンは %s に保持）", p.Old)
}

// SettleDelay は「新版が生き延びた」と判定するための稼働時間。
const SettleDelay = 30 * time.Second

// verifyStaged は一時保存ファイルを検証する：まず SHA256 を比較し、次に実際に起動して1回実行する。
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("チェックサムの読み取りに失敗しました：%w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("チェックサムの計算に失敗しました：%w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 が一致しません（ダウンロード時の破損または改ざん）")
	}
	return smokeTest(p.New)
}

// smokeTest は -h で新バイナリーを起動し、現在のシステム上で実行できることを確認する。
// ダウンロードの切断、アーキテクチャの選択誤り（exec format error）、依存関係の不足など、多くの問題を防げる。
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("実行権限の付与に失敗しました：%w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("動作確認がタイムアウトしました（新しい実行ファイルが応答しません）")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("動作確認に失敗しました：%v：%s", err, snippet)
	}
	return nil
}

// swap は現在のバイナリーを一時保存した新版に置き換える。
//
// Unix と Windows はどちらも実行中の実行ファイルの rename を許可する（Windows が禁止するのは削除と
// 上書きで、rename は含まれない）。そのためプラットフォーム別の処理も、自分自身の事前停止も不要。
func swap(p Paths) error {
	// Windows の rename は既存の移動先を上書きしないため、前回更新で残った .old を先に消去する必要がある。
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("古いバックアップ %s の削除に失敗しました：%w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("現在のバージョンのバックアップに失敗しました：%w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 置換に失敗し、現在の版をすでに移動していた場合は、元の場所にそのまま戻す必要がある。しないと次回起動時に実行ファイルがない。
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("新しいバージョンの配置に失敗し（%v）、現在のバージョンの復元にも失敗しました：%w", err, rerr)
		}
		return fmt.Errorf("新しいバージョンの配置に失敗しました：%w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback は swap でバックアップした旧版に戻す。
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("復元できるバックアップ %s がありません：%w", p.Old, err)
	}
	// 起動できない新版は直接削除せず、調査用に .failed へ移動する。
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("起動に失敗したバージョンの移動に失敗しました：%w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("前のバージョンの復元に失敗しました：%w", err)
	}
	return nil
}

// Rollback は /api/update/rollback の実装で、自発的に前の版へ戻す。
// 置換のみ行い、再起動は同様に監視スクリプトに任せる（呼び出し元は続いて ExitRestart で終了する）。
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("復元できる前のバージョンがありません（" + p.Old + " が存在しません）")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("前のバージョンを実行できないため、ロールバックを拒否しました：%w", err)
	}
	// 現在版とバックアップを交換し、ロールバック後にまた戻せるようにする。
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("現在のバージョンの移動に失敗しました：%w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("前のバージョンの配置に失敗しました：%w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] ロールバック後のバックアップ整理に失敗しました（動作には影響しません）：%v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup はロールバック可能な前の版の有無を返し、フロントエンドがロールバックボタンの表示を決めるために使う。
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "不明なバージョン"
	}
	return s
}
