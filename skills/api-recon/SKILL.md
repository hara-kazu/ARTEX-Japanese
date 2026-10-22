---
name: api-recon
description: ウェブサイトのAPIを収集するときに、このskillを呼び出します。
---

# API Recon（フロントエンドのインターフェース偵察）

**許可を得ている**ことを前提に、**バックエンド API**（パス、メソッド、パラメーター、応答本文）、**フロントエンドのルート**、**UI 機能の起動箇所**（Tab、ダイアログ、テーブル操作など）を、できるだけ網羅的に発見します。

---

## 範囲と禁止事項（Agent 必読 · 違反は範囲逸脱）

この skill は **API / パラメーターの偵察のみ**を行い、脆弱性の発見やペネトレーションテストによる悪用の段階ではありません。

### タスクの範囲

| 範囲 | 許可 | 禁止 |
|---|---|---|
| **目標** | path、method、パラメーター、ルート、UI 起動箇所の列挙 | SQLi/XSS/権限逸脱/総当たり/fuzz による脆弱性の調査、通信内容を改変する攻撃、破壊的な操作 |
| **認証** | Hook + stub/mock による**クライアント側**ログイン判定の回避 | ユーザーへのアカウント名・パスワードの要求や推測、実際のログインフォームの送信試行 |
| **実行時** | 認証情報なしでインターフェースを hook し、mock 応答で SPA をログイン後の画面の外枠に進める | 実際のバックエンドセッションがなければ続行できない処理 |

### 認証情報なしの動的解析（Phase 3 の既定）

1. `preload.js` / `runtime_harvest.js` で、ログイン、権限、メニューなどの bootstrap インターフェースを**捕捉して stub 化**します；
2. 業務データの照会インターフェースには、**構造が正しく、業務コードが成功を示し、データは空でもよい** mock body を返します；
3. バックエンドがない、または 401 の環境でもフロントエンドでログイン後のページを描画させ、より多くの XHR/fetch/WebSocket を発生させます；
4. **空データ、空のテーブル、仮の UI はいずれも想定どおり**です。これを理由に実際のログインや脆弱性テストへ移らないでください。

**一文で言えば**：mock でフロントエンドのルートを開き、コンポーネントをマウントさせ、**outbound リクエストだけを記録**します。バックエンドが何を返すかではなく、フロントエンドが**ほかにどのインターフェースを呼ぶか**が重要です。

### 手順上の厳禁事項

| 禁止 | 代わりに行うこと |
|---|---|
| Phase 1 完了前に grep/curl/Read で主 entry の `index-*.js` から API path を抽出 | `OUTDIR/harvest_static.py` を実行 |
| harvest の代わりに `extract_apis.py` などのスクリプトを自作 | `OUTDIR/harvest_static.py` を修正して再実行 |
| 同じ grep/コマンドが ≥2 回失敗しても繰り返す | 方針を変更：tool_logs を読む、harvest を修正、reference を確認 |
| 関門 A/B を飛ばし、`scripts/` の原版を直接実行 | OUTDIR にコピーして対象に合わせて修正 |
| 実際のユーザー名/パスワード、OTP、OAuth などによる認証 | stub/mock（上記参照） |
| 「実データを取得する」ために stub を省き、権限逸脱/注入テストを実行 | outbound のみを記録し、recon の範囲を守る |
| 削除、機密データのエクスポート、一括書き込みなどの不可逆な操作 | coverage でのクリックも同様 |
| runtime + 動的列挙を完了せずに、すべてのページとインターフェースを取得したと述べる | 「完了の定義」を参照するか、制約を明記 |
| パラメーターのトリガー表 + diff を完了せず、すべてのパラメーターを把握したと述べる | Phase 3b の表 + Phase 5 の diff |
| 一つの runtime サンプルから必須/任意を推定 | 複数サンプルの diff、または検証ルール/エラーからの推定 |

---

## 二層のモデル + 実行モード

| 層 | 成果物 | 限界 |
|---|---|---|
| **静的**（JS bundle） | endpoint の全パス、ルートの草案、リクエスト組み立て箇所のフィールド候補 | HTTP メソッドは不明。パラメーターには Phase 1b が必要。実行時に連結する URL は見落とす |
| **実行時**（動作中のセッション） | メソッド + body + 応答 + 動的 URL + WS/SSE。複数サンプルの diff でパラメーターを補完 | 実際にページを描画しないとリクエストが発生せず、一サンプルだけでは必須/任意を判断できない |

| 実行モード | エンジン | 用途 |
|---|---|---|
| **depth** | `runtime_harvest.js`（Puppeteer） | API 一覧、METHOD/params/応答本文、WS/SSE、再現可能な一括実行 |
| **coverage** | browser + `preload.js` | Tab/ダイアログ/テーブルをクリックし、機能の調査範囲を深める |
| **both** | depth の後に coverage | 最も網羅的で、所要時間も最長 |

**パラメーターの調査方法**（汎用スクリプトなし）：path には harvest/正規表現を使用し、パラメーターには **基準点から周辺を広げる解析 + UI のデータ連携の追跡 + 複数サンプルの diff + エラーからの推定**を使用します（grep の手順は [reference.md](reference.md) の J 節）。

---

## 完了の定義

次をすべて満たした場合だけ、recon 完了と述べられます：

- [ ] **静的**：Phase 1 harvest で `api_static.txt`、`routes.txt`、`js/` を生成
- [ ] **実行時**：depth または coverage の少なくとも一方を実施。coverage/both は **Hook の有効化 + 動的列挙の反復**が必要
- [ ] **画面の外枠への到達**：業務 path の閲覧時に `/login` ではない（hash ルートに注意）
- [ ] **パラメーター**：coverage/both でパラメーターのトリガー表 + `param_samples.json` を完成し、Phase 5 で `params_merged.json` を統合
- [ ] **深さ**（モジュールページが空白の場合）：Phase 4 で権限ツリーを復元して再実行し、**module 単位の API** が現れるまで実施（locale/bootstrap だけでは不可）
- [ ] **成果物**：Phase 5 の成果物をすべて生成（Phase 5 の成果物一覧を参照）。`insert_assets` でサービスとエンドポイントの資産を保存

---

## スクリプトと関門

`scripts/` は参考テンプレートにすぎません。原版を直接実行し、最終結果として扱うことは**禁止**です。

**ルール**：先に読む → 対象に合わせて修正 → `OUTDIR`（例：`recon/`）へ保存 → `CHANGES.md` に記録。一致しない場合は構造だけを参考に、調査方法に従って書き直します。

| 関門 | 実施時期 | 参考スクリプト → OUTDIR のコピー | よくある必須の修正箇所 |
|---|---|---|---|
| **A（静的）** | Phase 0 後、harvest/spider の**初回**実行前 | `harvest_static.py` / `spider_mpa.py` | **多くのサイトは既定の regex で直接実行可能**。manifest/表記が合わない場合だけ endpoint の正規表現、webpack/Vite の `publicPath`、MPA の exclude/cookie を修正 |
| **B（実行時）** | Phase 2 後、depth/coverage の実行前 | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage のキー、neutralize の成功値、stubs、login の正規表現、api の接頭辞、hash/history |

**SPA の必須順序**（入れ替え不可。「先に探索してからスクリプト」より Phase 番号を優先）：

| 手順 | 必須 | 禁止 |
|---|---|---|
| Phase 0 完了後 | 次の Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | 主 entry `index-*.js` への curl/grep/Read（通常 >500KB） |
| 関門 A | スクリプトをコピー → 必要に応じて小さく修正 → **直ちに実行** | 先に手作業で API を抽出してから harvest の要否を判断 |
| Phase 1 完了前 | `wc -l` で成果物を検証。404 は harvest を修正して再試行 | extract スクリプトの自作、未ダウンロードの URL に対する grep の反復 |
| Phase 1b 以降 | grep は `OUTDIR/js/*.js` のみ | 主 bundle を harvest の代わりに使用 |

- ✅ `harvest_static.py` をコピー → （任意）regex を修正 → **直ちに実行**
- ❌ 主 bundle を curl → grep を繰り返す → 一時的な extract を作成 → 最後に harvest
- **MPA**：Phase 0 後の次の Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## ツールと出力の制約

| 制約 | 説明 |
|---|---|
| 大きなファイル | >100KB の `index-*.js` を Read/grep でコンテキストに入れることは**禁止**。OUTDIR のスクリプトで一括処理 |
| grep の出力 | `\| head -20` または `-m 5` が必須。対話には path の要約だけを残し、bundle の断片を貼らない |
| 検証 | `wc -l`、`ls \| wc -l` を使用。ディレクトリ全体を Read しない |
| regex による初期調査 | 任意、≤1 回、≤50KB の小さな chunk または HTML のみ。正式な静的解析は harvest を基準にする |
| reference | 手順/テンプレート/トラブル対処は [reference.md](reference.md) を参照。全文を inline で繰り返さない |

---

## 実行の流れ

```
Phase 0 分類 + OUTDIR
  → 関門 A → Phase 1 harvest（★ 直ちに実行 ★）
  → Phase 1b パラメーターのリバース解析
  → Phase 2 認証の三つの関門 → config.json
  → 関門 B → Phase 3 実行時 + パラメーターのトリガー表
  → Phase 4 権限ツリー（必要な場合）→ Phase 3 を再実行
  → Phase 5 レポートを統合 + insert_assetsで発見したすべてのサービス、エンドポイントのapi資産を一括登録し、登録時は発見済みの資産を決して漏らさない
```

順番にチェックし、**前の項目が未完了のまま次の Phase へ進まないでください**。

1. [ ] **Phase 0**：SPA/MPA の初期調査。`OUTDIR` を作成 → [Phase 0](#phase-0--分類)
2. [ ] **関門 A + Phase 1**：スクリプトをコピー → **直ちに** harvest → `wc -l` で検証 → [Phase 1](#phase-1--静的)
3. [ ] **Phase 1b**：基準点から周辺を広げる解析 + データ連携の層 → `param_candidates.json` → [Phase 1b](#phase-1b--パラメーターのリバース解析)
4. [ ] **Phase 2**：認証の三つの関門 → `config.json` → [Phase 2](#phase-2--認証の三つの関門)
5. [ ] **関門 B**：runtime スクリプトを調整 → [Phase 3](#phase-3--実行時)
6. [ ] **Phase 3**：depth / coverage / both。画面の外枠への到達を確認し、パラメーターのトリガー表を作成 → `param_samples.json`
7. [ ] **Phase 4**（必要な場合）：権限ツリー → patch stubs → Phase 3 を再実行 → [Phase 4](#phase-4--権限ツリーの復元)
8. [ ] **Phase 5**：成果物を統合 + レポート + `insert_assets` → [Phase 5](#phase-5--統合とレポート)

---

## Phase 0 — 分類

入口の HTML を取得し、**`OUTDIR` を作成**します（skill 内の `scripts/` は変更しない）：

- **SPA**：空の外枠 + `<div id=app>` + chunk → Phase 1–5
- **MPA**：SSR + `<form>`、endpoint bundle なし → 関門 A の後：

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

`forms.txt`、`links.txt`、`api_inline.txt` を生成。SPA で forms ≈ 0 の場合 → Phase 1 へ切り替え。

---

## Phase 1 — 静的

[スクリプトと関門](#スクリプトと関門) · [ツールと出力の制約](#ツールと出力の制約)を守ってください。

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest：HTML script を解析 → webpack/Vite manifest → すべての lazy chunk をダウンロード → `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` を生成。

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk 数 vs manifest：404 は harvest を修正して再試行し、手動で chunk を一つずつ curl しない
- `api_static.txt` が少なすぎる → OUTDIR 内の endpoint の正規表現を広げて再実行（reference 参照）

### Phase 1b — パラメーターのリバース解析

path は Phase 1 から取得し、パラメーターのフィールドは別途 recon が必要です。grep のルールは [ツールと出力の制約](#ツールと出力の制約)を参照してください。

**完了条件**：重要なインターフェースについて、フィールド名、送信位置、型の推定、必須かどうか、サンプル値、確信度を答えられること。

#### 1b.0 — 通信の形式

| 形式 | パラメーターの場所 | 静的解析で優先して見る箇所 |
|---|---|---|
| REST JSON | body + query | path の基準点の周辺にある `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql テンプレート、`$page: Int` |
| 従来の form | urlencoded | `<form>`、`FormData` |
| ファイルのアップロード | multipart | `FormData.append` |
| パスパラメーター | `/user/:id` | ルートの表 + `useParams` / `$route.params` |
| 暗号化/署名 | `sign`/`data` に格納 | 暗号化関数の引数を Hook（reference D 節） |

成果物：各インターフェースに `transport: query|json|form|graphql|encrypted` を明記。

#### 1b.1 — 基準点から周辺を広げる解析

既知の path を基準点に、確認範囲を広げてリクエストの組み立てオブジェクトを探します：

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| ラッパーの層 | パラメーターの手掛かり |
|---|---|
| axios のインスタンス | `data` / `params` |
| 共通の request | インターセプターが全体共通のフィールドを追加 |
| OpenAPI クライアント | 生成された method のシグネチャ |
| React Query / SWR | hook の第二引数 |
| Vue composable | composable の引数 |

型の情報の残存：`yup`/`zod`/rules、`Form.Item name=`、埋め込みの Swagger。

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — データ連携の層

```
Form field → onFinish/handleSubmit → transform → API payload
```

| データ連携元 | 方法 |
|---|---|
| フォームの submit | submit → transform → API を追跡 |
| テーブルの検索 | `getFieldsValue()` → `params` |
| ルート | `:id` / `?tab=` |
| インターセプター | 全体共通の `tenantId`、ページ分割、sign |
| 列挙値の select | `options` → API の列挙値 |

DevTools の call stack で `fetch`/`XHR.send` から呼び出し元をたどり、リクエストの組み立て関数を探します。

#### 1b.3 — リクエスト組み立ての三つの問い（≠ Phase 2 の認証の三つの関門）

| 問い | 答える内容 |
|---|---|
| **組み立て** | payload の build 箇所、transform の形跡 |
| **検証** | required、pattern、enum |
| **送信** | path / query / body / multipart / ヘッダー |

インターセプターの関門（Phase 2）では、全体共通で追加するフィールド（Authorization、`X-Tenant-Id`、sign）も読み取ります。

#### 1b.4 — Phase 3 への引き継ぎ

候補のフィールドは静的解析/データ連携の層から取得します。**必須/任意/条件の依存関係**には、Phase 3 のパラメーターのトリガー表 + diff + Phase 5 のエラーからの推定が必要です。

---

## Phase 2 — 認証の三つの関門

`OUTDIR/js/` で grep（`head` 付き）し、`config.json` に記録します（手順は reference）：

| 関門 | 問い | キーワード |
|---|---|---|
| **描画の関門** | ログイン済みかどうかをどう判断するか？ | `isLogin`、`getToken`、Cookie/localStorage |
| **インターセプターの関門** | 何を契機に `/login` へ移動するか？ | `response_code`、`errno`、axios interceptor |
| **内容の関門** | メニュー/権限はどこから取得するか？ | `menu`、`permission`、`role`、`acl`、`routes` |

localStorage のキー名を認証情報とみなすことは禁止です。chunk/リクエストの連鎖から確認してください。

**次の段階 = 関門 B**：結論を `config.json` に記録し、`OUTDIR/runtime_harvest.js` / `preload.js` を修正します。

### Phase 2b — API の観察（任意）

OUTDIR 内の `preload.js` で、セッションのキー名、Authorization、入れ子の API URL を確認します：

| 設定 | 成果物 |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers の観察 |
| `extractUrlsFromResponse: true` | 応答内の下位 API |
| `observe.storageReads/cookieReads: true` | config に反映 |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage の各回で書き出すもの：`__API_RECON_LOG__`、`__API_RECON_DETAIL__`、`__API_RECON_ROUTES__`、`__API_RECON_OBSERVE__`。

---

## Phase 3 — 実行時

関門 B を通過済みであること。[範囲と禁止事項](#範囲と禁止事項agent-必読--違反は範囲逸脱) · 認証情報なしの mock 方針を守ってください。

`config.json` に `"runtimeMode": "depth" | "coverage" | "both"` を設定します（テンプレートは reference）。

### Hook と stub（depth + coverage 共通）

| 層 | 範囲 | 目的 |
|---|---|---|
| L1 完全一致 | auth/権限/bootstrap stub | 初期画面の認証を通過 |
| L2 失敗応答の修正 | すべての JSON 応答 | 未ログインのコード → 成功 |
| L3 最終手段 | L1 に一致しない `/api` など | 空の成功応答で UI を描画 |

- **depth**：fake auth + `forward` で業務コードを修正 + `stubs`。`routes` を巡回（hash/history）し、`runtime_api.json` を生成
- **coverage**：**document-start** で `preload.js` を追加（CDP の `addScriptToEvaluateOnNewDocument` または Userscript）

検証：`window.__API_RECON_PRELOAD__` が存在し、業務 path が `/login` へ戻らないこと。

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage の動的列挙（必須）

1. メインナビゲーション/サイドバー — 各項目をクリックし、ネットワークを 1–3s 待つ
2. Tab — `role=tab`、`.ant-tabs-tab`
3. テーブル — 先頭行の表示/編集/詳細
4. ツールバー — エクスポート、絞り込み、新規作成（**不可逆な削除を避ける**）
5. 各モジュールに入るたび — API/ルートを統合
6. SPA — `routes.txt` にある未巡回の path に制御された `pushState` を実行（MPA は禁止）

**パラメーターのトリガー表**（必須）：各モジュールで操作の種類ごとに一度記録し、**複数サンプルを diff**：

| 操作 | 通常増えるパラメーター |
|---|---|
| 一覧の初期画面 | ページ分割 + 既定の絞り込み |
| 検索をクリック | keyword、filter |
| 高度な絞り込み | より多くの optional |
| 新規作成/編集 | 完全な entity |
| 一括操作/エクスポート/並べ替え | `ids[]`、`exportType`、`sortField` |

**stub 使用時も outbound の body/headers は実際のもの**です。リクエストを基準にしてください。記録 → `scan_raw.json`、`param_samples.json`、`api_detail.json`。

- **Vue**：`neutralizeVueRouter: true` + document-start preload
- **React**：`routes.txt` + サイドバーのクリック + `pushState`
- **both**：先に 3a depth、次に 3b coverage

---

## Phase 4 — 権限ツリーの復元

**実施条件**：モジュールページが空白 / 各ルートが bootstrap（例：locale）のみ → 内容の関門を通過できていない。

| 状況 | 意味 |
|---|---|
| 画面の外枠への到達に成功 | 描画の関門 + インターセプターの関門を通過済み |
| サイドバーに項目が足りない/クリックすると空白 | stub の shape または権限コードが不足 |
| 各ルートの API が同じで極端に少ない | `v-if permission` を通過できていない |
| `routes.txt` が bundle より大幅に少ない | auth モジュールから補完が必要 |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

典型的な連鎖：`role_permissions`（flat codes）+ `permissions/all`（tree）→ `getResultTree` → `userRouteAuth[CODE].url`。

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

中間成果物：`route_map.json`、`userRouteAuth.json`、`permissions_tree.json`、`*_stub.json`、`perm_codes_all.txt`。

stub の確認：外側の `response_code` がインターセプターの関門と一致し、flat codes と tree が対応し、`routes` が `route_map` のすべての link を含むこと。

`config.json` を更新後、**Phase 3 を再実行**します。大規模な SPA は `waitUntil`、`routeTimeout`、`perRouteMs` を調整できます（reference A3/I 節）。

---

## Phase 5 — 統合とレポート

### 成果物一覧

| ファイル | 段階 | 内容 |
|---|---|---|
| `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` | 1 | 静的 bundle と path |
| `param_candidates.json` | 1b | 静的なパラメーターのフィールド候補 |
| `config.json` | 2 | 三つの関門 + runtime 設定 |
| `runtime_api.json` | 3a | depth の詳細記録（WS/SSE を含む） |
| `param_samples.json`、`scan_raw.json`、`api_detail.json` | 3b | 複数サンプル、クリックのログ、detail |
| `route_map.json` など | 4 | 権限ツリーの中間ファイル（実施した場合） |
| `params_merged.json` | 5 | パラメーターのフィールド統合 + 確信度 |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | ルート、API、params、機能、制約 |
| **insert_assets** | 5 | すべてのサービス、エンドポイントの資産を資産データベースへ保存 |

### 5b — パラメーターの統合

`param_samples.json` から diff します。**汎用の統合スクリプトはありません**。確信度のルールは reference J7（高/中/低/トリガー待ち）を参照してください。

### 5c — エラーからの推定

許可された範囲内で、不完全なリクエストを送信して 400 を読むことができます（**パラメーターの recon であり、脆弱性テストではありません**）：`field 'x' is required`、列挙値のエラーなど。`data` のラッパー、`variables`、暗号化前の `bizData` に注意してください。

レポートには、runtimeMode、静的/実行時の API 数、パラメーターの確信度、未調査モジュール、参考スクリプトに対する `CHANGES.md` の要約を明記してください。

`site_map.json` の推奨構造：

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

その他のフィールドと grep の手順は [reference.md](reference.md) を参照してください。

---

## 共通の説明

- **フレームワークに依存しない**：webpack/Vite/Angular の lazy load は同じ方法
- **通信**：REST/JSON、GraphQL、WebSocket、SSE。gRPC-web は対象外
- **SSR**：クライアント側の fetch は記録可能。RSC/Server Actions は完全には列挙できない
- **解析の死角**：JSVMP、WASM、HMAC/mTLS による厳密な検証 → 静的解析 + 制約の明記
- **パラメーターの解析の死角**：条件間の連動、hidden params、WASM でのリクエスト組み立て → 「トリガー待ち」/「到達不能」
- **静的解析は取りこぼしを補う**：runtime が遮断されても静的解析で endpoint を列挙可能

---

## 追加リソース

- Grep の手順、`config.json` のテンプレート、トラブル対処、Hook、パラメーターのリバース解析 J 節、site_map のテンプレート：**[reference.md](reference.md)**
- 参考スクリプトのパスは [スクリプトと関門](#スクリプトと関門) の表を参照
