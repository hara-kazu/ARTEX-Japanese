import { localizeApiError, localizeApiErrorBody, localizeApiResponse } from "./api-errors.ts";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import ts from "typescript";

test("backend validation messages retain dynamic values", () => {
  assert.equal(localizeApiError("密码长度至少 8 位"), "パスワードは8文字以上にしてください");
  assert.equal(localizeApiError("关联任务 #42 不存在"), "関連タスク #42 が存在しません");
  assert.equal(localizeApiError('未知传输方式 "custom"'), '不明な通信方式 "custom"');
  assert.equal(
    localizeApiError("变量 {{.TaskID}} 不在该 agent 允许列表"),
    "変数 {{.TaskID}} は、この agent で使用できません",
  );
  assert.equal(localizeApiError("模板语法错误: unexpected EOF"), "テンプレートの構文エラー：unexpected EOF");
  assert.equal(localizeApiError("未提供 API Key"), "API Key が未指定です");
  assert.equal(localizeApiError("token 生成失败"), "token の生成に失敗しました");
});

test("nested errors retain diagnostic details", () => {
  assert.equal(
    localizeApiError("安装失败：文件过大：sample.zip"),
    "インストールに失敗しました：ファイルが大きすぎます：sample.zip",
  );
  const diagnostic = "GET /tasks: 502 gateway unavailable";
  assert.equal(localizeApiError(diagnostic), diagnostic);
  assert.equal(localizeApiError("下载 file.zip: socket closed"), "file.zip のダウンロード：socket closed");
  assert.equal(localizeApiError("安装失败：task not found"), "インストールに失敗しました：task not found");
});

test("resource names and response bodies retain text that matches known diagnostics", () => {
  for (const value of ["任务不存在", "文件不存在", "密码不能为空"]) {
    assert.equal(
      localizeApiError(`company 不存在: ${value}（先用 list_companies 确认，或建好企业）`),
      `company が存在しません：${value}（list_companies で確認するか、企業を作成してください）`,
    );
    assert.equal(localizeApiError(`读取 ${value}: 任务不存在`), `${value} の読み込み：タスクが存在しません`);
    assert.equal(localizeApiError(`未知工具类型: ${value}`), `不明なツールの種類：${value}`);
    assert.equal(localizeApiError(`文件过大：${value}`), `ファイルが大きすぎます：${value}`);
    assert.equal(localizeApiError(`API 返回 500: ${value}`), `API の応答 500：${value}`);
    assert.equal(
      localizeApiError(`解析 Telegram 响应失败: 任务不存在 (${value})`),
      `Telegram の応答を解析できません：タスクが存在しません（${value}）`,
    );
    assert.equal(
      localizeApiError(`项目 ${value} 详情获取失败: 任务不存在`),
      `プロジェクト ${value} の詳細を取得できません：タスクが存在しません`,
    );
  }
  assert.equal(
    localizeApiError("invalid task asset: 第 2 条范围无效: 无效 IP: 文件不存在"),
    "invalid task asset：第2件の範囲が無効です：無効な IP：文件不存在",
  );
  assert.equal(localizeApiError("请求失败: 任务不存在"), "リクエストに失敗しました：タスクが存在しません");
  assert.equal(localizeApiError("保存失败: 任务不存在"), "保存に失敗しました：タスクが存在しません");
  assert.equal(
    localizeApiError("冒烟测试失败: 任务不存在: 文件不存在"),
    "動作確認に失敗しました：タスクが存在しません：文件不存在",
  );
  assert.equal(
    localizeApiError("装入新版本失败(任务不存在)，且恢复当前版本失败: 文件不存在"),
    "新バージョンの配置に失敗し（タスクが存在しません）、現在のバージョンの復元にも失敗しました：ファイルが存在しません",
  );
});

test("error bodies preserve unchanged JSON and non-JSON content exactly", () => {
  for (const body of [
    '{"error":"database is locked"}\n',
    '{\n  "error": "socket closed", "status": 502, "detail": "gateway unavailable"\n}\n',
    "503 Service Unavailable\r\n",
  ]) {
    assert.equal(localizeApiErrorBody(body), body);
  }
  assert.equal(
    localizeApiErrorBody('{"error":"未知工具类型: 文件不存在","status":400}\n'),
    '{"error":"不明なツールの種類：文件不存在","status":400}',
  );
});

test("ambiguous argument boundaries preserve the complete message", () => {
  for (const message of [
    "读取 任务不存在: 文件不存在: socket closed",
    "下载 文件不存在: 任务不存在: socket closed",
    "公司 任务不存在 创建失败: 文件不存在 创建失败: socket closed",
    "冒烟测试失败: 任务不存在: 文件不存在: socket closed",
    "保存失败: 读取 任务不存在: 文件不存在: socket closed",
  ]) {
    assert.equal(localizeApiError(message), message);
  }
  assert.equal(
    localizeApiError("读取 文件不存在: socket closed"),
    "文件不存在 の読み込み：socket closed",
  );
  assert.equal(
    localizeApiError("公司 文件不存在 创建失败: 任务不存在"),
    "会社 文件不存在 を作成できません：タスクが存在しません",
  );
});

test("tool configuration and upload failures preserve English bodies and status", async (t) => {
  const source = await readFile(new URL("./api.ts", import.meta.url), "utf8");
  const compiled = ts.transpileModule(source, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, removeComments: true },
  }).outputText
    .replace('from "@/lib/api-errors"', `from ${JSON.stringify(new URL("./api-errors.ts", import.meta.url).href)}`)
    .replace('import { MOCK } from "@/lib/mock/enabled";', "const MOCK = false;")
    .replace('import { mockHandle } from "@/lib/mock/handler";', "const mockHandle = () => { throw new Error(); };");
  const { api } = await import(`data:text/javascript,${encodeURIComponent(compiled)}`);
  const body = '{\n  "error": "database is locked", "status": 503\n}\n';
  const requests = [];
  t.mock.method(globalThis, "fetch", async (url, init) => {
    requests.push([url, init?.method ?? "GET"]);
    return new Response(body, { status: 503 });
  });
  await assert.rejects(api.interceptGetToolConfig(), (error) => error.message === body);
  await assert.rejects(api.interceptSetToolConfig(["bash"]), (error) => error.message === body);
  await assert.rejects(
    api.chatUpload("task", "42", []),
    (error) => error.message === `アップロードに失敗しました: 503 ${body}`,
  );
  assert.deepEqual(requests, [
    ["/api/intercept/tool-config", "GET"],
    ["/api/intercept/tool-config", "PUT"],
    ["/api/chat/upload?scope=task&id=42", "POST"],
  ]);
});

test("English diagnostics remain unchanged, including templates and prefixes", () => {
  for (const message of [
    "task not found",
    "invalid JSON: unexpected EOF",
    "description must be at most 4000 characters",
    "provider init failed: socket closed",
    "skill already exists",
    "GET /tasks: 502 gateway unavailable",
  ]) {
    assert.equal(localizeApiError(message), message);
  }
  const payload = {
    error: "task not found",
    warnings: ["database unavailable"],
    items: [{ last_error: "provider init failed: socket closed" }],
  };
  assert.equal(localizeApiResponse(payload), payload);
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
