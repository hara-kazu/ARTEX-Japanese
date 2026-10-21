import { localizeApiError, localizeApiResponse } from "./api-errors.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("backend validation messages retain dynamic values", () => {
  assert.equal(localizeApiError("密码长度至少 8 位"), "パスワードは8文字以上にしてください");
  assert.equal(localizeApiError("关联任务 #42 不存在"), "関連タスク #42 が存在しません");
  assert.equal(localizeApiError('未知传输方式 "custom"'), '不明な通信方式 "custom"');
  assert.equal(
    localizeApiError("变量 {{.TaskID}} 不在该 agent 允许列表"),
    "変数 {{.TaskID}} は、このエージェントで使用できません",
  );
  assert.equal(localizeApiError("模板语法错误: unexpected EOF"), "テンプレートの構文エラー：unexpected EOF");
});

test("nested errors retain diagnostic details", () => {
  assert.equal(
    localizeApiError("安装失败：文件过大：sample.zip"),
    "インストールに失敗しました：ファイルが大きすぎます：sample.zip",
  );
  const diagnostic = "GET /tasks: 502 gateway unavailable";
  assert.equal(localizeApiError(diagnostic), diagnostic);
  assert.equal(localizeApiError("下载 file.zip: socket closed"), "file.zip のダウンロード：socket closed");
});

test("successful and partial responses localize diagnostics without changing user content", () => {
  const payload = {
    ok: false,
    error: "未获取到模型列表",
    items: [
      { id: 42, ok: false, error: "任务不存在" },
      { id: 43, ok: true },
    ],
    warnings: ["LLM 配置 5 已删除，配置链项已跳过"],
    description: "任务不存在",
    tool_input: { error: "任务不存在" },
  };
  const result = localizeApiResponse(payload);
  assert.equal(result.error, "モデル一覧を取得できませんでした");
  assert.equal(result.items[0].error, "タスクが存在しません");
  assert.equal(result.items[1], payload.items[1]);
  assert.equal(result.warnings[0], "LLM 設定 5 は削除済みのため、設定チェーンから除外しました");
  assert.equal(result.description, payload.description);
  assert.equal(result.tool_input, payload.tool_input);
  assert.equal(payload.error, "未获取到模型列表");
  assert.equal(localizeApiResponse([{ last_error: "任务不存在" }])[0].last_error, "タスクが存在しません");
});
