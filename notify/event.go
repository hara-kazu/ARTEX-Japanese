package notify

// Snapshot は notification_events.snapshot という JSONB 列の契約。書き込み側は db 層の
// 脆弱性保存トランザクション、読み取り側は server 層の配信エンジンとフィルター照合。定義を本パッケージに置くのは、これが
// 「通知ドメイン」のペイロードだから。db はシリアライズのみを担当し、フィールドの意味は解釈しない。
//
// 描画時に再取得せず脆弱性フィールドを重複保存する理由：脆弱性は後から名称、レベル、状態が変更されるが、
// プッシュ内容は**発生時点**の結論を反映すべきで、再取得すると「後から low に変更された」という
// 危険な誤解を招く。また、fan-out と描画で findings/tasks/assets の3つの表を JOIN する必要もなくなる。
type Snapshot struct {
	// イベントの種類：finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed の場合のみ空でない。
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item はプッシュ待ちの脆弱性1件で、チャネルの描画に使う。
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets は解決済みのアセット表示名（ドメイン/IP など）。server 層が設定する。
	// 本パッケージはデータベースに触れず、名前を取得できないため。
	Assets []string
	// DetailURL は脆弱性詳細へのリンク。空なら public_base_url 未設定を示し、描画時に省略する。
	DetailURL string
	// 状態変更イベント専用。両方空でなければ「未対応 → 修正済み」と描画する。
	FromStatus string
	ToStatus   string
}

// IsStatusChange は項目が状態変更イベントかどうかを返す。
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title は項目の表示タイトルを返す：手動で付けた name を優先し、なければ脆弱性の種類 vulnclass を使い、
// 両方空ならプレースホルダーを使う。空のタイトルは決して出力しない。
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "（名前のない脆弱性）"
}

// Message はチャネルによる1回の送信内容全体。
type Message struct {
	// 単発プッシュでは長さ 1、集約プッシュ（digest）ではバッチ全体となる。
	// 空のスライスは不正で、呼び出し元は少なくとも1件を保証する必要がある。
	Items []Item
	// Batch=true なら集約メッセージとして描画する（タイトルを変え、時間範囲と件数を含める）。
	Batch bool
	// WindowMinutes は集約周期（分）で、Batch=true の場合のみ「直近 N 分」という文言に使う。
	// 描画時に time.Since で計算せず、意図的に設定から明示的に渡す。描画の決定性を保つことでテストしやすくする。
	WindowMinutes int
	// HomeURL はプラットフォームのダッシュボードアドレス（グローバルの public_base_url）。空ならダッシュボードへの入口を付けない。
	HomeURL string
}
