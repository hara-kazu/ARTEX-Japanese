# api-recon — 参考マニュアル

Grep の手順、`config.json` のテンプレート、トラブル対処。grep はすべて `js/` ディレクトリで実行します。bundle が一行の場合は先に `js-beautify` または `sed 's/}/}\n/g'` を使用できますが、通常は前後の確認範囲を指定した raw grep で十分です。

## スクリプトの説明

`scripts/` 内のすべてのファイルは**参考テンプレート**であり、実行前に対象サイトに合わせて調整する必要があります。典型的な変更箇所：

| スクリプト | よくある調整箇所 |
|---|---|
| `harvest_static.py` | endpoint の正規表現、webpack/Vite manifest の解析、マイクロフロントエンドの publicPath、再試行/並列実行 |
| `runtime_harvest.js` | neutralize のフィールド名と成功値、stub の照合ルールと body の構造、routes の取得元、WS の記録、`waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`、L1 stubs、`neutralize.fields`、`apiPattern`、L3 を有効にするか、`recordDetail`、`observe.*`、`neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` で破壊的なリンクを除外、cookie、depth/max、同一ドメインの絞り込み |
| `extract_route_map.py` | `routeMap` / `routeLink` の正規表現、KEY の命名形式 |
| `build_perm_tree.py` | `userRouteAuth` の解析、`ROOTS`/`PREFIX_PARENT` の階層を推定する規則、stub の外側のフィールド名 |
| `config.json` | 上記すべてのサイト固有パラメーターをまとめる入口 |

調整したファイルはタスクの作業ディレクトリ（例：`recon/`）に保存し、参考スクリプトからの具体的な変更点をレポートに明記することを推奨します。

---

## A. リバース解析の三つの関門

### A1. 描画の関門 — 「ログイン済みかどうかをどう判断するか？」

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

`isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` の連鎖を探し、**ストレージのキー**、**保存先**（Cookie vs localStorage）、**エンコード**を確認します：

| エンコード | config での偽装方法 |
|---|---|
| 平文の文字列 / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | 署名なし/`alg:none` の JWT、または bundle 内の鍵で署名 |
| 暗号化（SM2/AES/RSA） | 埋め込みの鍵を探す。描画の関門がデコード可能な blob だけを必要とする場合は forge が可能。それ以外は静的解析で補う |

→ `cookies` / `localStorage` に記録。

### A2. インターセプターの関門 — 「何を契機に /login へ移動するか？」

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

**フィールド名**、**成功値**（通常 `0` または `200`）、**移動を発生させる失敗値**を確認します。junk session で検証：

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ `neutralize.fields` + `neutralize.success` に記録。

### A3. 内容の関門 — 「メニュー/権限はどこから取得するか？」

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**二層のデータ**（よくある企業向け管理画面）：

| API | 典型的な payload | 利用側 |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | ルートのガード、ボタン単位の ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | サイドバーのメニュー描画 |
| bundle 内の `userRouteAuth` | `{ CODE: { url, name? } }` | code → フロントエンドの path |
| bundle 内の `routeMap` | `{ KEY: { name, link } }` | 別名の解決（webpack の `o.DASHBOARD`） |

利用側のコードを読み、`getResultTree(tree, permissions)` がどう絞り込むか、`v-if` / `hasAuth(code)` がどのフィールドを確認するかを調べます。

**手作業の forge**（小規模サイト）：permissive payload を組み立て → `stubs`。

**完全な権限ツリーの復元**（大規模サイトでサイドバー/下位モジュールがまだ空白の場合）：**I 節**を参照。

---

## B. config.json のテンプレート

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

フィールドの説明：
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` の接頭辞：`b64json:` → base64(JSON)。`json:` → 元の JSON。接頭辞なし → リテラル
- `forward: true` は実際のリクエストを転送してコードのフィールドを書き換え、`false` は完全にオフラインの stub
- `mockTier`：coverage モードで preload が有効にする層。例：`L1+L2`、`L1+L2+L3`
- `routes` は `routes.txt` から取得。メニューを forge すると harness が `<a href>` を自動追加
- `captureResponses` / `recordWs` は depth モードでのみ有効
- `waitUntil`：大規模な SPA は `domcontentloaded` を使用し、`networkidle2` で待機し続けるのを避ける
- `routeTimeout`：各ルートの `page.goto` のタイムアウト（ミリ秒）
- `proxy`：Puppeteer の `--proxy-server`。`HTTP_PROXY` / `HTTPS_PROXY` でも設定可能

### B1. 二つの stub のテンプレート（role_permissions + permissions/all）

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

外側のフィールド名（`response_code` / `code` / `data`）は A2 のインターセプターの関門と一致させ、`permissions` は tree 内のすべての leaf code を含める必要があります。

---

## C. coverage モード：preload の設定

`scripts/preload.js` の先頭の `CONFIG` オブジェクトを編集するか、CDP で追加する前に置換します：

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* config.json の stubs と同じ */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

検証：`window.__API_RECON_PRELOAD__ === true` で、pathname が安定していること。

記録結果の書き出し：

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime の Hook 機能

preload（coverage）と runtime_harvest（depth）に組み込まれたブラウザーの Hook 機能と対応範囲：

| Hook 機能 | API 発見に役立つ点 | 対応範囲 |
|---|---|---|
| Hook fetch / XHR.open | リクエストの URL/メソッドを記録 | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Authorization などのヘッダーを発見 | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie の読み取り | セッションのキー名を確認 | ⚠️ 任意の `observe.storageReads/cookieReads` |
| Vue のルート取得 | frontendRoutes を補完 | ✅ `__API_RECON_ROUTES__`（読み込み済みルート） |
| Vue のルートガードの無効化 / ログインへの移動の遮断 | モジュールを描画して API を発生させる | ✅ `neutralizeVueRouter` + 標準の移動処理の無効化 |
| React のルート取得 | ルートを補完 | ⚠️ 静的解析 + クリック。専用の Hook はなし |
| ページ移動の遮断（ログイン path） | 同じページで解析 | ⚠️ 業務のナビゲーションを妨げないよう、ログイン path のみ遮断 |
| Hook 暗号化ライブラリ（CryptoJS/SM など） | 暗号化されたパラメーター → 平文の API body | ❌ 暗号化関数の引数を手作業で Hook し、結論を config に記録する必要あり |
| デバッグ妨害の bypass | 対応しないと runtime が API を記録できない | ❌ 手作業での対処が必要。静的解析は引き続き利用可能 |

---

## E. Endpoint 抽出の正規表現（静的解析で少なすぎる場合）

`harvest_static.py` の `extract_endpoints` の条件を広げるか、手作業で実行：

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. トラブル対処

| 状況 | 原因 → 対処 |
|---|---|
| 静的な API が少ない | endpoint の表記が一致していない → 正規表現を広げる（D 節） |
| chunk 数 ≪ manifest | CSS-only、または未配備の chunk。404 は再試行済み |
| runtime でまだログイン画面が出る | 描画の関門が誤っている → A1 のキー名、保存先、エンコード、domain を再確認 |
| 画面の外枠には入れるがモジュールが空白 | 内容の関門 → メニューを forge（A3）。`routes` の path が違う可能性 |
| 各ルートが bootstrap/locale のみ | 権限コードが不足 → I 節の権限ツリー復元。`role_permissions` + `permissions/all` の二つの stub を確認 |
| サイドバーに項目はあるが下位ページが空白 | tree の intermediate ノードが不足、または code と `userRouteAuth` が不一致 |
| どの API もログインへ移動する | インターセプターの関門 → `neutralize` を確認。入れ子のフィールドには walk の処理の拡張が必要 |
| WS フレームが 0 | ユーザー操作後に subscribe する必要がある。`perRouteMs` を延長 |
| 応答本文が空 | `forward: true` の場合だけ実際の応答がある |
| Chromium がない | chromium をインストールするか、`config.chromium` / `CHROMIUM` を設定 |
| Mock が多いのにログインへ戻る | Hook が遅すぎるか、`location.href` の setter がない → document-start + preload |
| 一覧がすべて空 | L3 の空配列は正常。Tab/設定/詳細のクリックを続行 |
| Redux の action をルートと誤認 | get/set/change/clear/toggle/upload を含む内部の path を除外 |
| Vue がまだログインへ移動する | preload が document-start ではない → 追加するタイミングを変更。`neutralizeVueRouter: false` の場合は手動でガードを解除 |
| 応答内に URL があるが log に入らない | `extractUrlsFromResponse` を有効にするか、`__API_RECON_DETAIL__` から手作業で抽出 |
| Authorization のヘッダー名が不明 | `observe.xhrHeaders` を有効にするか、DevTools でリクエストヘッダーを確認 |
| runtime が極端に遅い / タイムアウト | `waitUntil: domcontentloaded` に変更し、`routeTimeout` を下げる。`networkidle2` は使わない |
| プロキシ接続が失敗 | `proxy` / 環境変数を確認。Puppeteer と curl のプロキシポートを一致させる |

---

## G. hardened な対象

サーバーがセッションを段階的に検証する場合（forge できない署名付き cookie、サーバーで描画し stub 化できないメニュー）、runtime は shell で停止します。想定する動作：

- **静的解析で endpoint の列挙は十分に可能** — モジュールの path はコード内にある
- 許可の範囲内なら、**実際のセッション**で同じ harness を実行：`forward: true`、neutralize は不要。実際の methods/params/responses を取得

---

## H. 一回のタスクのチェックリスト

1. 許可された範囲を確認
2. `scripts/harvest_static.py` を**読む** → 対象に合わせて調整 → 実行 → `api_static.txt`、`routes.txt` を確認
3. **Phase 1b**：path の基準点から周辺を広げる解析 + データ連携の層 → `param_candidates.json`（J 節）
4. A1/A2/A3 をリバース解析 → サイト固有の `config.json` を作成
5. `runtime_harvest.js` / `preload.js` を**読んで調整してから**実行
6. `runtimeMode=depth`：`npm install` → 調整済みの harvest スクリプトを実行
7. `runtimeMode=coverage/both`：document-start で調整済みの preload を追加 → browser MCP の動的列挙 + **パラメーターのトリガー表**
8. モジュールが描画されない → **I 節の権限ツリー復元** → patch stubs → 再実行
9. パラメーターの複数サンプルの diff + エラーからの推定 → `params_merged.json`
10. 統合 → `site_map.json` + `api_merged.txt`。調査範囲、不足箇所、スクリプトの変更点を正直に明記

---

## I. 権限ツリーの復元（Phase 4 の詳細化）

単純な `menus: [{ path, show: true }]` の forge が機能せず、下位モジュールがまだ mount しない場合に使用します。

### I1. auth モジュールの特定

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

記録：**権限 API path**、**応答のフィールド名**、**利用側の chunk ファイル名**。

### I2. routeMap の抽出

```bash
python3 scripts/extract_route_map.py recon/js recon/
# recon/route_map.json を生成
```

`[!] no routeMap pattern found` の場合：`extract_route_map.py` の正規表現を広げるか、手動で grep：

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. 権限ツリー + stub の構築

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

スクリプトの処理：
1. `userRouteAuth={MONITOR:{url:...},...}` を解析（webpack の別名 `He=o.DASHBOARD` を含む）
2. `route_map.json` で alias → 実際の path を解決
3. code の接頭辞から parent を推定（`MONITOR_ALERT` → `MONITOR`）
4. `permissions_tree.json`、`permissions_all_stub.json`、`role_permissions_stub.json` を出力
5. `--config` の場合は `config.json` の `stubs` に自動で記録し、`routes` を拡張

**対象に合わせて調整**（スクリプトの先頭）：
- `DEFAULT_ROOTS`：最上位のモジュールの code 一覧
- `DEFAULT_PREFIX_PARENT`：`PREFIX_` → parent の対応
- `DEFAULT_EXTRA_PARENT`：接頭辞の関係に当てはまらない orphan ノード

### I4. stub の整合性の検証

```bash
# permissions の数 ≈ userRouteAuth の項目数であること
wc -l recon/perm_codes_all.txt
# routes が route_map のすべての link を含むこと
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. runtime の再実行と比較

```bash
node recon/runtime_harvest.js recon/config.json
# forge 前後の runtime_api.json の件数を比較し、/attack、/asset などのモジュール API が現れるか確認
```

| forge 前 | forge 後（成功） |
|---|---|
| 各ルートが同じ 3–5 件の bootstrap | ルートごとに異なる module API が発生 |
| `/api/locale/language` のみ | `/api/web/...` のモジュール endpoint が現れる |
| `routes.txt` は一桁のルート数 | `routes` は route_map からの 80–110+ 件 |

### I6. まだ失敗する場合

- **coverage モード**：サイドバー + Tab をクリック。権限の gating により、操作後に初めてリクエストする可能性
- **stub のフィールド**：実際の API（curl + 実際の session）と stub の nesting を比較
- **追加のガード**：grep で `hasPermission|checkRole|func.` などのボタン単位の確認を探し、`role_permissions.permissions` を拡張
- **静的解析で補う**：モジュールの API path は引き続き `api_static.txt` 内にある。runtime は METHOD/body の補完のみ。パラメーターは `param_candidates.json` + 記録済みサンプルを保持

---

## J. パラメーターのリバース解析（Phase 1b / 5b / 5c）

**調査方法であり、汎用スクリプトではありません。** path は正規表現で探し、パラメーターは基準点から周辺を広げる解析 + UI のデータ連携の追跡 + 複数サンプルの diff + エラーからの推定で調べます。

### J1. 基準点から周辺を広げる解析 — path から組み立てオブジェクトを探す

```bash
# Phase 1 の既知の path を基準点にする
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. ラッパーの層と通信の形式

```bash
# axios / 共通の request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# パスパラメーター
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. 検証の関門 — 必須 / 形式 / 列挙値

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. データ連携の層 — フォーム → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime の補完：DevTools → Network → リクエスト → **開始元**（call stack）で `fetch`/`send` から呼び出し元をたどり、リクエストの組み立て関数を探します。

### J5. 暗号化されたパラメーター

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**暗号文からフィールドを推測しない**でください。暗号化関数の**引数**を Hook し、暗号化前に plaintext payload を記録します。結論は `config.json` / `param_candidates.json` に記録してください。

### J6. パラメーターのトリガー表（Phase 3 で必須）

各モジュールで操作ごとに一度記録し、リクエストの body/query を diff します：

| 操作 | 確認事項 |
|---|---|
| 一覧の初期画面 | ページ分割の既定値 |
| 検索 | keyword、filters |
| 高度な絞り込み | optional のフィールド |
| 新規作成/編集 | 完全な entity |
| 一括操作/エクスポート | `ids[]`、`exportType` |
| 並べ替え/ページ移動 | `sortField`、`order` |

`param_samples.json` を生成：`[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. 確信度のルール

| 確信度 | 条件 |
|---|---|
| **高** | 静的な callsite + runtime の ≥2 サンプルが一致 |
| **中** | 静的解析のみ、または runtime が 1 回のみ |
| **低** | 応答/エラーからの推定で、再検証していない |
| **トリガー待ち** | 静的に判明しているフィールドで、UI/権限により未到達 |

### J8. 状況別の簡単な設定

| 状況 | 順序 |
|---|---|
| REST の一覧ページ | J1 の組み立てオブジェクト → J6 の四回の diff → J3 の rules |
| 新規作成/編集フォーム | J3 の Form name → J4 の submit の連鎖 → runtime の送信 + 意図的に空にして 400 を確認 |
| GraphQL | J2 の variables の宣言 → runtime の各 operation で variables を記録 |
| 暗号化された body | J5 の Hook 引数 → 暗号化前のフィールドが実際の params |

### J9. api-recon の段階との対応

| api-recon | パラメーターの recon |
|---|---|
| Phase 1 静的 | J1 の基準点から周辺を広げる解析 |
| Phase 2 A2 インターセプター | 全体共通で追加するフィールド（tenantId、sign） |
| Phase 3 runtime | J6 のトリガー表 + `param_samples.json` |
| Phase 4 権限ツリー | モジュールごとにフォームが異なる → 十分な権限があって初めて全フィールドが発生 |
| Phase 5 統合 | `params_merged.json` + 確信度。一サンプルで必須と判断しない |

### J10. トラブル対処

| 状況 | 対処 |
|---|---|
| 静的にはフィールド名があるが runtime で現れない | 「トリガー待ち」と明記。権限ツリーを補完 / 高度な絞り込みをクリック / 連動する select の各 option を操作 |
| 同じ path で body の形が異なる | 正常。`action` ごとに分けて記録し、無理に schema を統合しない |
| stub の応答は仮だが params を見たい | **outbound リクエストの** body/headers を確認し、stub 応答から逆算しない |
| 400 が nested field を報告 | 外側のラッパー `data`/`bizData`/`variables` に注意 |
| GraphQL で operation 名しか見えない | `variables` の JSON を展開し、静的解析で `$var: Type` を探す |

---
