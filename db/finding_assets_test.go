package db

import (
	"strconv"
	"strings"
	"testing"
)

// cleanupTreeFixtures はケースが作成した資産と検出結果を削除する。必ず defer で登録する(
// t.Cleanup ではなく):t.Cleanup はテスト関数から戻った後に実行され、その時点で defer d.Close() が接続を閉じているため、
// 削除は黙って失敗し、共有開発 DB に不正データが残る。
func cleanupTreeFixtures(d *DB, taskID int64, rootDomains ...string) {
	d.Exec(`DELETE FROM assets WHERE root_domain = ANY($1::text[])`, rootDomains) //nolint:errcheck
	d.DeleteFindingsByTask(taskID)                                                //nolint:errcheck
}

// seedTreeAsset inserts one asset row.
func seedTreeAsset(t *testing.T, d *DB, kind string, cols map[string]any) int64 {
	t.Helper()
	names := []string{"type"}
	values := []any{kind}
	placeholders := []string{"$1"}
	for k, v := range cols {
		values = append(values, v)
		names = append(names, k)
		placeholders = append(placeholders, "$"+strconv.Itoa(len(values)))
	}
	q := "INSERT INTO assets(" + strings.Join(names, ",") + ") VALUES (" +
		strings.Join(placeholders, ",") + ") RETURNING id"
	var id int64
	if err := d.QueryRow(q, values...).Scan(&id); err != nil {
		t.Fatalf("seed %s asset: %v", kind, err)
	}
	return id
}

func nodeByKey(tree *FindingAssetTree, key string) *FindingAssetNode {
	for i := range tree.Nodes {
		if tree.Nodes[i].Key == key {
			return &tree.Nodes[i]
		}
	}
	return nil
}

// TestBuildFindingAssetTree covers the whole shape of the「資産別」tree: the
// root→subdomain→service→endpoint chain gets rebuilt from a finding that only
// points at the leaf, ancestors aggregate their subtree, assets without any
// finding stay out, and a finding whose asset row is gone lands in the
// unassigned bucket.
func TestBuildFindingAssetTree(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tk, err := d.CreateTask("资产树测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	const root = "tree-test.example"
	const sub = "api.tree-test.example"
	defer cleanupTreeFixtures(d, tk.ID, root)
	rootID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": root, "root_domain": root})
	subID := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": sub, "root_domain": root})
	svcID := seedTreeAsset(t, d, "service", map[string]any{
		"domain": sub, "root_domain": root, "url": "https://" + sub, "port": 443, "service_type": "http",
	})
	epID := seedTreeAsset(t, d, "endpoint", map[string]any{
		"domain": sub, "root_domain": root, "url": "https://" + sub + "/admin", "port": 443, "method": "GET",
	})
	// 同じドメインの別サービスで、検出結果を付けない —— ツリーに現れないこと。
	seedTreeAsset(t, d, "service", map[string]any{
		"domain": sub, "root_domain": root, "url": "http://" + sub + ":8080", "port": 8080, "service_type": "http",
	})

	// 最深部の endpoint のみに検出結果を付け、祖先チェーンはツリー構築で補完すること。
	if _, err := d.AddFinding(tk.ID, 0, "XSS", "反射型 XSS", "high", "s", "e", "w", []int64{epID}); err != nil {
		t.Fatal(err)
	}
	// サービスに直接付けた1件で、Self と Total の違いを検証する。
	if _, err := d.AddFinding(tk.ID, 0, "Info", "信息泄露", "low", "s", "e", "w", []int64{svcID}); err != nil {
		t.Fatal(err)
	}
	// 資産行なし(削除済み資産)→ 未関連バケット。
	if _, err := d.AddFinding(tk.ID, 0, "Misc", "孤儿", "medium", "s", "e", "w", []int64{999000111}); err != nil {
		t.Fatal(err)
	}

	tree, err := d.BuildFindingAssetTree(FindingFilter{TaskID: strconv.FormatInt(tk.ID, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if tree.FindingTotal != 3 {
		t.Fatalf("finding_total: want 3, got %d", tree.FindingTotal)
	}

	rootNode := nodeByKey(tree, assetKey(rootID))
	subNode := nodeByKey(tree, assetKey(subID))
	svcNode := nodeByKey(tree, assetKey(svcID))
	epNode := nodeByKey(tree, assetKey(epID))
	for name, n := range map[string]*FindingAssetNode{
		"root": rootNode, "subdomain": subNode, "service": svcNode, "endpoint": epNode,
	} {
		if n == nil {
			t.Fatalf("%s node missing from tree", name)
		}
	}

	// 親子チェーン:endpoint → service → subdomain → root_domain。
	if epNode.Parent != svcNode.Key {
		t.Errorf("endpoint parent: want %s, got %s", svcNode.Key, epNode.Parent)
	}
	if svcNode.Parent != subNode.Key {
		t.Errorf("service parent: want %s, got %s", subNode.Key, svcNode.Parent)
	}
	if subNode.Parent != rootNode.Key {
		t.Errorf("subdomain parent: want %s, got %s", rootNode.Key, subNode.Parent)
	}
	if rootNode.Parent != "" {
		t.Errorf("root parent: want top level, got %s", rootNode.Parent)
	}

	// 集計:ルートドメインは2件(endpoint の high + service の low)、service 自身は1件、サブツリーは2件。
	if rootNode.Total != 2 || rootNode.High != 1 || rootNode.Low != 1 {
		t.Errorf("root totals: want 2/high1/low1, got %d/high%d/low%d", rootNode.Total, rootNode.High, rootNode.Low)
	}
	if rootNode.Self != 0 {
		t.Errorf("root self: want 0 (祖先にすぎない), got %d", rootNode.Self)
	}
	if svcNode.Total != 2 || svcNode.Self != 1 {
		t.Errorf("service total/self: want 2/1, got %d/%d", svcNode.Total, svcNode.Self)
	}
	if epNode.Total != 1 || epNode.Self != 1 {
		t.Errorf("endpoint total/self: want 1/1, got %d/%d", epNode.Total, epNode.Self)
	}

	// 検出結果がない兄弟サービスはツリーに含めない。
	for _, n := range tree.Nodes {
		if n.Label == "http://"+sub+":8080" {
			t.Errorf("asset without findings should be hidden: %+v", n)
		}
	}

	// 未関連バケットは削除済み資産を指す検出結果を受け入れる。
	none := nodeByKey(tree, FindingUnassignedAsset)
	if none == nil || none.Total != 1 || none.Medium != 1 {
		t.Fatalf("unassigned bucket: want 1 medium, got %+v", none)
	}
}

// TestFindingAssetScopeFilter verifiesノード1つの選択が narrows the findings list to
// that node's whole subtree, and that the unassigned sentinel works too.
func TestFindingAssetScopeFilter(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tk, err := d.CreateTask("资产筛选测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	const root = "scope-test.example"
	const sub = "api.scope-test.example"
	const other = "other-scope-test.example"
	defer cleanupTreeFixtures(d, tk.ID, root, other)
	rootID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": root, "root_domain": root})
	subID := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": sub, "root_domain": root})
	otherID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": other, "root_domain": other})

	if _, err := d.AddFinding(tk.ID, 0, "A", "子域名上的", "high", "s", "e", "w", []int64{subID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddFinding(tk.ID, 0, "B", "别的根域名上的", "high", "s", "e", "w", []int64{otherID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddFinding(tk.ID, 0, "C", "没有资产的", "high", "s", "e", "w", nil); err != nil {
		t.Fatal(err)
	}
	// 削除済み資産を指す検出結果は、asset_ids が空のものと同様に「未関連」に属する——ツリーのバケットが受け入れ、
	// 一覧フィルターでも検索できること。基準が一致しないと、バケットの数値が開いた後の件数より大きくなる。
	if _, err := d.AddFinding(tk.ID, 0, "D", "资产已删除", "high", "s", "e", "w", []int64{999000333}); err != nil {
		t.Fatal(err)
	}

	base := FindingFilter{TaskID: strconv.FormatInt(tk.ID, 10)}
	cases := []struct {
		name  string
		scope string
		want  int
	}{
		{"サブツリー全体", assetKey(rootID), 1},      // ルートドメイン配下にはサブドメインの1件のみ
		{"リーフノード", assetKey(subID), 1},       // サブドメイン自身
		{"別のツリー", assetKey(otherID), 1},     // 相互に混在しない
		{"未関連", FindingUnassignedAsset, 2}, // asset_ids が空のもの + 削除済み資産を指すもの
		{"存在しないノード", "a:999000222", 0},       // 現在のフィルターに当該ノードがない → 空の結果とし、フィルターなしにはしない
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			f.AssetScope = tc.scope
			items, total, err := d.ListFindingsPage(f, 1, 50)
			if err != nil {
				t.Fatal(err)
			}
			if total != tc.want || len(items) != tc.want {
				t.Fatalf("scope %s: want %d findings, got total=%d items=%d", tc.scope, tc.want, total, len(items))
			}
		})
	}

	// scope なしでは4件すべてが存在する。
	if _, total, err := d.ListFindingsPage(base, 1, 50); err != nil || total != 4 {
		t.Fatalf("unscoped: want 4, got %d (%v)", total, err)
	}

	// ツリーの未関連バケットの件数と、開いた後に検索される件数は一致すること —— これこそ両者の基準が分かれると失敗するアサーション。
	tree, err := d.BuildFindingAssetTree(base)
	if err != nil {
		t.Fatal(err)
	}
	none := nodeByKey(tree, FindingUnassignedAsset)
	if none == nil || none.Total != 2 {
		t.Fatalf("unassigned bucket count: want 2, got %+v", none)
	}
}
