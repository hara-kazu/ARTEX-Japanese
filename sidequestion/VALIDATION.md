# `/btw` の検証記録

日付：2026-09-10。ブランチ：`codex/btw-side-question`。ベースライン：`8dae851b9b622f2ff2631f332fde9719d0b16fba`。

独立した PostgreSQL のテスト用データベースとデータディレクトリを使用しました。実際のモデルの認証情報は独立したテスト環境にのみ渡し、コードやこの記録には書き込まず、製品の既定モデルも変更していません。Go 1.26.3、norma v0.3.6、Next.js 16.2.9。

実際のモデルとの対話、戻り値のオブジェクト、実装上の検証、Qwen の元のレビュー文は [validation-2026-09-10.json](validation-2026-09-10.json) に保存しており、API の認証情報は含みません。

## 実装の検査

| 範囲 | 結果 | 証拠 |
| --- | --- | --- |
| 構造化メッセージとツール引数のディープコピー | 合格 | `TestCheckpointDeepCopyAndBoundaries` |
| 要約 / 圧縮リクエストで上書きしないこと、完全な応答と最終状態の公開、途中の応答の除外 | 合格 | `TestCheckpointDeepCopyAndBoundaries`、`TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 実際のモデルプールのメンバーの識別 | 合格 | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| ツールの対応、20組の再提示、予算に応じた削減、上限超過エラー | 合格 | `TestBuildRequestCompactionToolPairingAndBudget` |
| メインとサイド質問の並行実行、双方向の取り消しの分離 | 合格 | 処理を待機させる Provider、`TestMainSideConcurrencyAndIndependentCancellation` |
| ツールを実行しないこと、ストリーミング / 非ストリーミング、失敗時の取得済み使用量 | 合格 | `TestServiceNoToolsAndUsageOnFailure` |
| 実際の norma ChatAgent + ローカルの Read ツール、メイン transcript / アクティビティの分離 | 合格 | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`、ストリーミングと非ストリーミングのサブケース |
| 永続化、ページ分割、冪等性、再起動時の部分回答の保持 | 合格 | `TestSideHistoryIdempotencyPagingAndRecovery` |
| 消去と遅れて届く書き込みの競合、親リソースの削除、バージョンの比較 | 合格 | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker のアーカイブと復元、v1/v2/v3 | 合格 | `TestSideTaskArchiveVersions` |
| 3種類の親インターフェース、認証、リソースの所属、Worker の論理削除 | 合格 | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`、`TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| メインセッションが処理中でもサイド質問できること、独立した SSE の再接続 / 切断、取り消し、消去 | 合格 | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 親セッションごとに1件 / 全体で4件の同時実行 | 合格 | 2つの `TestSideHTTP…` ケース |
| 送信前のスナップショットの保存、再起動後の追加質問、古いセッションのスナップショットの偽装禁止 | 合格 | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| キャッシュ内の設定の削除やモデルの変更後の続行拒否 | 合格 | `TestSideRejectsDeletedOrChangedCachedProfile` |
| アーカイブ前の取り消しと、最終回答・使用量の保存完了の待機 | 合格 | `TestSideTaskDrainPersistsBeforeArchive` |
| ストリームの利用側が早期に取り消した場合の、使用量とサイド質問の所属の1回だけの記録 | 合格 | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| 再起動により自動復元した Worker / deadline の実行コンテキストが、新しいスナップショットの公開を続けること | 合格 | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| 関連パッケージの race 検査 | 合格 | 以下のコマンド |
| TypeScript と本番ビルド | 合格 | `npx tsc --noEmit`、`npm run build` |
| 追加したフロントエンドモジュールの Biome | 合格 | `biome check`、追加した3モジュール |

独立した破棄可能なデータベースを `ARTEX_PG_DSN` に設定すると、自動チェックを再現できます（本番データベースを指定しないでください）。

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

Go の全体回帰テストはすべて成功したわけではありません。`server` パッケージの既存の2テストが、一時ディレクトリの削除時に失敗し、いずれも `TempDir RemoveAll … directory not empty` を返しました。

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

上記の未変更のベースラインからソースを出力し、同じ分離環境で `server` パッケージを再実行したところ、これら2つの削除失敗も再現しました。ベースラインの実行では、さらに `TestCoreTaskLifecyclePG` の目標ノード数の検証が失敗しましたが、最終修正後の `server` の回帰テストでは、この検証の失敗はありませんでした。他のパッケージ、今回のサイド質問の関連ケース、race 検査は合格しました。ベースラインの問題を今回の検証の合格と扱わず、問題を隠すために既存の検証条件を変更することもしていません。

Next.js のビルドでは、既存の複数の lockfile / workspace root の推定に関する警告が出ました。ビルドは完了し、すべてのページの生成に成功しました。

## ブラウザーの検査

Codex In-app Browser を使用し、独立したローカルの Go サービスと Next.js の開発サーバーに接続しました。デスクトップと390 × 844の狭い画面で、以下の手動・自動操作を行い、スクリーンショットとブラウザーのログを確認しました。

- 通常のチャットの実行中に `/btw` を入力し、メインの内容とサイド質問を同時表示。デスクトップのサイドバーは正常でした。
- 続けて追加質問を実施。サイド質問の停止後は生成済みの部分を保持し、メインの処理は続行しました。
- パネルを閉じてもリクエストは続行し、再び開くと完了した回答を復元しました。ページの更新後、空の `/btw` で履歴を復元しました。
- 狭い画面の Drawer の入力、ボタン、履歴、閉じる操作は正常で、横方向にはみ出しませんでした。
- 消去には確認ダイアログを使用し、消去後は履歴が消え、メイン transcript とスナップショットは保持されました。
- タスクの MainAgent と2つの Worker でそれぞれ質問して切り替え、Agent のラベルと履歴が混ざらないことを確認しました。
- 処理を待機させるローカルモデルのテスト用フィクスチャで Worker の実行を維持し、Worker のメイン入力欄から `/btw` を送信しました。サイド質問の停止後も、Worker は実行中の状態と自身の一時停止ボタンを表示し続け、サイド質問は部分回答を保存しました。
- ブラウザーのエラー / 警告ログは空でした。

制御可能なテスト用フィクスチャを使い、実際のモデルの出力速度に依存せず、同時実行のタイミングを正確に検証しました。デバッグ中の2回の Worker の実行時検査では、有効な同時実行の期間を作れませんでした（タスクが終了 / 回答が先に完了）。テスト用フィクスチャを修正して再実施し、合格しました。これら最初の操作は、有効な合格として数えていません。

## 実際のモデルとの対話

まず `grok-4.6` を検査し、OpenAI 互換インターフェース `http://127.0.0.1:12580/tingly/openai` を使用しました。検査は HTTP 200 を返し、モデル名 `grok-4.6` と `READY` を返すまでに2.82秒かかりました。第一候補が使用可能だったため、Tingly の `glm` と智譜の `glm-5.3` の予備チェーンは使用せず、この2つの予備サービスは今回検証していません。

| 場面 | 実際の結果 |
| --- | --- |
| メインセッションの実行中に資産、目標、目印を質問 | `redhaze.top`、トップページの読み取りと要約という目標、`BTW-REAL-0910` を返しました。サイド質問は16.97秒で完了しました |
| メインセッションのトップページ読み取り完了後に、ツールによる根拠を質問 | WebFetch 200、curl のリダイレクト301 → 302 → 200、ページのタイトルを正しく引用しました。7.24秒 |
| サイド質問で Bash によるテストファイルの作成を要求 | 実行を拒否し、対象のファイルは作成されませんでした。7.74秒 |
| 完了後のサイド質問がメインコンテキストを変更しないこと | メイン transcript の SHA-256 とメインのアクティビティ記録は変化せず、サイド質問のツール実行回数は0でした |
| 実際に Go サービスを停止 / 再起動した後の追加質問 | 以前のサイド質問の履歴3件を保持し、メイン Agent を再実行せず、永続化したスナップショットから資産、目印、タイトルを直接回答しました |
| 新しいセッションで Grok の非ストリーミング設定を使用 | 資産と `ATOMIC-0910` を正しく回答しました。使用量を返して保存しました：input 11734、output 138、cache_read 11520 |

資産の例のメインセッションでは、WebFetch と Bash/curl で公開されたトップページを読み取り、最終的なページは `https://id.redhaze.top/home`、タイトルは「红幕科技 RedHaze Group · 全球综合集团门户」でした。Bash は応答をローカルのテストファイルに一時保存し、リモートへの書き込みは行っていません。この事実は、「サイド質問ではツールを実行していないこと」と分けて検証しました。

メイン transcript の検証値：`e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`。

**使用量の制限：** Tingly の Grok のストリーミング応答は usage を返しませんでした。別途、`stream_options.include_usage=true` を直接送信して確認したところ、HTTP 200、データフレーム12件、usage フレーム0件でした。そのため、ストリーミングテストの0は、エンドポイントが使用量を提供しなかったことを表し、課金されていないとは解釈できません。非ストリーミングの使用量と、テスト用フィクスチャの失敗 / 取り消し時の使用量は、いずれも正しく保存しました。

## Qwen のレビュー

レビューモデルは `qwen-flash`、OpenAI 互換インターフェースは `https://dashscope.aliyuncs.com/compatible-mode/v1` で、HTTP 200 でした。最初の3件の実際のサイド質問の対話、メインセッションのツールによる根拠、実装上の検証を渡したところ、`verdict: accept`、`concerns: []` を返し、回答は資産、目印、ページの読み取りの証拠と一致し、サイド質問のツール実行の拒否は制約に従っていると判定しました。レビューの使用量：prompt 6625、completion 312、total 6937。

今回の Qwen のレビュー範囲には、後で追加したサービスの再起動と非ストリーミングのテストを含みません。Qwen の「書き込みなし」という要約は広すぎました。メインセッションの curl は実際にローカルの応答の一時ファイルを作成しており、上記に明記しています。同時実行、ツール実行ゼロ、transcript の分離は実装上の検証で判断し、モデルのレビューは回答品質の評価を補助するためだけに使用しました。
