package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Repo は公開元。設定項目にせず固定する：更新元を設定可能にすると、設定を変更できる誰にでも
// リモートコード実行経路を与えることになり、侵入テストプラットフォームでは開放できない。
const Repo = "Hinln/ARTEX"

// GitHubTokenEnv はバックエンドプロセスのみが読み取り、フロントエンドや設定 API を通じてトークンを渡さない。
const GitHubTokenEnv = "ARTEX_UPDATE_GITHUB_TOKEN"

// latestURL は GitHub の「最新正式版」API。prerelease と draft を自動でスキップする。
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts は更新経路がアクセスできるドメインを制限する。以下の checkRedirect と併せて、
// 途中のどの遷移でもリスト外のホストへリダイレクトされたら即失敗する。DNS 汚染 / 中間者による
// バイナリー置換を防ぐ第1の関門で、第2は SHA256SUMS の照合。
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release アセットの実際の保存先となるオブジェクトストレージ
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release は GitHub Release 内の必要なフィールド。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset は Release に添付された1つのファイル。
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	APIURL string `json:"url"`
	Size   int64  `json:"size"`
}

// 非公開リポジトリーの添付ファイルは Release Asset API で認証を付けてダウンロードする必要がある。
func (a Asset) downloadURL() string {
	if a.APIURL != "" {
		return a.APIURL
	}
	return a.URL
}

// setUpdateAuth は固定の公開リポジトリーの HTTPS API にのみトークンを送る。添付ファイルが
// GitHub オブジェクトストレージへリダイレクトされる際は署名 URL を使い、Authorization を一緒に転送してはならない。
func setUpdateAuth(req *http.Request) {
	req.Header.Del("Authorization")
	u := req.URL
	if u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "api.github.com") ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil ||
		!strings.HasPrefix(strings.ToLower(u.EscapedPath()), strings.ToLower("/repos/"+Repo+"/releases/")) {
		return
	}
	if token := strings.TrimSpace(os.Getenv(GitHubTokenEnv)); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// NewClient は GitHub ドメインのみを受け付ける HTTP クライアントを作る。proxy が空なら直接接続する。
//
// デフォルトの Transport を意図的に再利用しない：更新経路は TLS の使用と証明書検証を強制する必要があり、他の場所で設定された
// InsecureSkipVerify などの影響を受けてはならない。
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // パッケージ全体をダウンロードするため、リクエスト単位のタイムアウトで止めてはならない
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("リダイレクトが多すぎます")
			}
			if err := checkURL(req.URL); err != nil {
				return err
			}
			setUpdateAuth(req)
			return nil
		},
	}
}

// checkURL は https + ドメイン許可リストを強制する。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 以外のアドレスを拒否しました：%s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 以外のドメインを拒否しました：%s", u.Hostname())
	}
	return nil
}

// FetchLatest は最新正式版を照会する。
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")
	setUpdateAuth(req)

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub に接続できません（システム設定で全体のプロキシを設定可能）：%w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("GitHub 認証に失敗しました。バックエンドの環境変数 %s を確認してください", GitHubTokenEnv)
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("GitHub がアクセスを拒否しました。トークンのリポジトリ Contents 読み取り権限または API 上限を確認してください")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub インターフェースのレート制限です。しばらくしてから再試行してください")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("リポジトリ %s にアクセスできないか、正式リリースがありません。非公開の場合はバックエンドに %s を設定してください（Contents 読み取り権限が必要）", Repo, GitHubTokenEnv)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub の応答：%d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release を解析できません：%w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release に tag がありません")
	}
	return &rel, nil
}

// AssetName は現在のプラットフォームに対応する公開パッケージ名を返し、build.sh の package_binary と一致させる：
// artex-<バージョン>-<os>-<arch>.zip（バージョン番号に v 接頭辞は付かない）。
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset は Release 内で名前によりアセットを探す。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
