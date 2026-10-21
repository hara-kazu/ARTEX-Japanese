import { localizeApiError } from "./api-errors.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("backend validation messages retain dynamic values", () => {
  assert.equal(localizeApiError("密码长度至少 8 位"), "パスワードは8文字以上にしてください");
  assert.equal(localizeApiError("关联任务 #42 不存在"), "関連タスク #42 が存在しません");
  assert.equal(localizeApiError('未知传输方式 "custom"'), '不明な通信方式 "custom"');
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
