package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 概要「目標管理」の手動 CRUD インターフェース。agent の set_goals と同じ goal ノードへ書きますが、
// ここでは人間が UI で直接追加・削除・変更します。追加/変更後は「タスクの復活」（admitTask resume：
// 最終状態→running、一時停止解除、必要なら待機）を再利用し、削除では復活させません（製品仕様）。各変更 handler は
// beginTaskOperation/decInflight を使い、タスク削除との競合を防ぎます（インテント CRUD と同じ）。

// listGoals はこのタスクの全目標を返します（text/vulnclass/state を分離済み）。目標管理カードの描画用。
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal は目標を手動追加します。DB 保存（タスクのルートの spawns 配下）→「目標追加」を記録して
// planner を起動 → タスクを復活させ、新目標に基づいて達成を再判定させます。
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、目標を追加できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "目標の内容を入力してください")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 「人が目標を追加：…」を記録し、planner を起動
	s.reviveTask(t)              // 完了済み/一時停止のタスクを実行中へ戻して続行
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "保存後に目標を読み取れませんでした")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal は目標本文（と vulnclass）を手動変更します。DB 更新 →「ユーザーが old から new へ変更」を
// 記録して planner を起動 → タスクを復活させ、新目標に基づいて方向を調整させます。
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、目標を変更できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "目標の内容を入力してください")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "目標が存在しません")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 「人が old から new へ目標を変更」を記録し、planner を起動
	s.reviveTask(t)                   // 追加と同様にタスクを復活させ、新目標を再判定
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "更新後に目標を読み取れませんでした")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal は目標を手動削除します（物理削除、辺/アンカーを連鎖削除）。DB 削除 →「ユーザーが目標 X を削除」を
// 記録して planner に残りの目標を再判定させます。製品仕様により削除ではタスクを復活【させません】。
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、目標を削除できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "目標が存在しません")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 「人が目標を削除：…」を記録し、planner を起動（タスクは復活させない）
	writeJSON(w, 200, map[string]bool{"ok": true})
}
