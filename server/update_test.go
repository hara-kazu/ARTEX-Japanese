package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache は GitHub の割当枠を保護する層：未認証 API は 60 回/時/IP のみで、
// 上部バーの"新しいバージョンあり"の表示はページ全体の読込ごとに照会する。キャッシュが機能しないと、ユーザーが複数の
// タブを開くだけで割当枠を使い切り、実際に更新したいときに照会できなくなる。

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 回照会しても取得元への問い合わせは 1 回だけであるべきですが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// ユーザーが「更新を確認」を押したらリアルタイムの結果を取得すること。そうしないと公開直後の版はキャッシュ期限まで見えない。
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force はキャッシュを迂回すべきで、取得元への問い合わせは 2 回を期待しましたが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 保存時刻を、ちょうど期限切れになる過去の時点へ戻し、TTL 到達を再現する。
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL の期限切れ後は取得元へ再度問い合わせるべきで、2 回を期待しましたが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github 不可达")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返ることを期待しました")
	}
	// 失敗結果も短時間キャッシュし、GitHub に到達できないときにページ読込ごとにタイムアウトを待つことを防ぐ。
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返ることを期待しました")
	}
	if calls != 1 {
		t.Errorf("エラーは短時間キャッシュすべきで、取得元への問い合わせは 1 回を期待しましたが、実際は %d 回でした", calls)
	}

	// ただしエラーの TTL は成功時より明確に短くし、ネットワーク復旧後すぐに自己回復できること。
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("エラーの TTL(%v) は成功時の TTL(%v) より短くする必要があります", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返ることを期待しました")
	}
	if calls != 2 {
		t.Errorf("エラーの TTL の期限切れ後は再試行すべきで、2 回を期待しましたが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 訪問者がタブを閉じるとリクエストはキャンセルされる。GitHub の問題ではないため、"キャンセル済み"を決して
	// キャッシュに書かないこと——そうしないと以降 30 分間、全訪問者が理由不明のエラーを受け取る。
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // キャッシュを期限切れにし、取得元へ照会させる

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("呼び出し元が取り消し済みの場合はエラーをそのまま返すべきです")
	}

	// 重要な不変条件：キャンセルされた1回は痕跡を一切残さず、キャッシュには"キャンセル済み"エラーがなく、
	// 前回の正常な結果を保持していること。
	if c.err != nil {
		t.Fatalf("取り消しエラーはキャッシュに書き込むべきではありませんが、%v でした", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("キャッシュは直前の正常な結果を保持すべきですが、%+v でした", c.rel)
	}

	// そのキャンセルでは新データを取得できなかったので、次の訪問者は取得元へ再照会すべきで、正常に結果を取得でき、
	// 前回のキャンセルの影響を受けないこと。
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("取り消し後の正常なリクエストはエラーになるべきではありません: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("正常な結果を取得すべきですが、%+v でした", rel)
	}
}
