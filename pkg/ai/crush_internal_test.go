//go:build (!freebsd && !openbsd && !netbsd && !dragonfly) || (freebsd && (amd64 || arm64)) || (openbsd && (amd64 || arm64))

package ai

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nghiant03/wakatime-cli/pkg/heartbeat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func crushFixtureHome(t *testing.T) (string, string) {
	t.Helper()

	home := updateTestHome(t)
	t.Chdir(home)

	for _, key := range []string{"CRUSH_DATA_DIR", "CRUSH_GLOBAL_DATA", "XDG_DATA_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, "")
	}

	global := filepath.Join(home, "global-crush")
	t.Setenv("CRUSH_GLOBAL_DATA", global)

	return home, global
}

func crushFixtureDB(t *testing.T, dir string) *sql.DB {
	t.Helper()

	db := updateDB(t, filepath.Join(dir, "crush.db"))
	updateSQL(t, db, `CREATE TABLE sessions (
		id TEXT PRIMARY KEY, parent_session_id TEXT, title TEXT NOT NULL,
		message_count INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL, created_at INTEGER NOT NULL, summary_message_id TEXT, todos TEXT, channel TEXT
	);
	CREATE TABLE messages (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, role TEXT NOT NULL, parts TEXT NOT NULL DEFAULT '[]',
		model TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, finished_at INTEGER,
		provider TEXT, is_summary_message INTEGER NOT NULL DEFAULT 0,
		prism_model_id TEXT, prism_model_name TEXT, prism_hypercredit_savings REAL, prism_dollar_savings REAL,
		FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
	);`)

	return db
}

func crushFixtureRegistry(t *testing.T, global string, projects ...map[string]any) {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"projects": projects})
	require.NoError(t, err)
	updateWrite(t, filepath.Join(global, "projects.json"), string(raw))
}

func crushFixtureProject(path, data string) map[string]any {
	return map[string]any{"path": path, "data_dir": data, "last_accessed": "2026-10-01T12:00:00Z"}
}

func crushFixtureSession(t *testing.T, db *sql.DB, id, parent string, at int64) {
	t.Helper()

	updateSQL(t, db, `INSERT INTO sessions
		(id,parent_session_id,title,created_at,updated_at) VALUES(?,?,?,?,?)`, id, parent, "Implement parser", at, at)
}

func crushFixtureMessage(t *testing.T, db *sql.DB, id, session, role, model, parts string, at int64, summary bool) {
	t.Helper()

	updateSQL(t, db, `INSERT INTO messages
		(id,session_id,role,model,provider,parts,created_at,updated_at,is_summary_message)
		VALUES(?,?,?,?,?,?,?,?,?)`, id, session, role, model, "anthropic", parts, at, at, summary)
}

func crushFixturePart(kind string, data map[string]any) map[string]any {
	return map[string]any{"type": kind, "data": data}
}

func crushFixtureParts(t *testing.T, parts ...map[string]any) string {
	t.Helper()

	raw, err := json.Marshal(parts)
	require.NoError(t, err)

	return string(raw)
}

func crushFixtureCall(t *testing.T, id, name string, input map[string]any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(input)
	require.NoError(t, err)

	return crushFixturePart("tool_call", map[string]any{
		"id": id, "name": name, "input": string(raw), "finished": true, "provider_executed": false,
	})
}

func crushFixtureResult(id, name, content, metadata string, failed bool) map[string]any {
	return crushFixturePart("tool_result", map[string]any{
		"tool_call_id": id, "name": name, "content": content, "metadata": metadata,
		"data": "", "mime_type": "", "is_error": failed,
	})
}

func TestCrushParsePromptsAndGeneratedMessages(t *testing.T) {
	home, global := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	crushFixtureRegistry(t, global, crushFixtureProject(project, data))
	db := crushFixtureDB(t, data)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()
	crushFixtureSession(t, db, "main", "", at)
	crushFixtureMessage(t, db, "prompt", "main", "user", "", `[
		{"type":"text","data":{"text":"Fix café 世界"}},
		{"type":"text","data":{"text":"generated continuation","hidden":true}},
		{"type":"image_url","data":{"url":"https://example.invalid/image.png"}}
	]`, at, false)
	crushFixtureMessage(t, db, "hidden", "main", "user", "",
		`[{"type":"text","data":{"text":"Continue automatically","hidden":true}}]`, at+1, false)
	crushFixtureMessage(t, db, "summary", "main", "user", "",
		`[{"type":"text","data":{"text":"Generated conversation summary"}}]`, at+2, true)
	crushFixtureMessage(t, db, "system", "main", "system", "",
		`[{"type":"text","data":{"text":"System instructions"}}]`, at+3, false)
	crushFixtureMessage(t, db, "shell", "main", "user", "",
		`[{"type":"text","data":{"text":"Generated shell context"}},`+
			`{"type":"shell_command","data":{"command":"pwd","output":"/project","exit_code":0}}]`, at+5, false)
	crushFixtureMessage(t, db, "report", "main", "user", "",
		`[{"type":"text","data":{"text":"Generated report context"}},`+
			`{"type":"sub_agent_report","data":{"text":"Subagent findings"}}]`, at+6, false)
	crushFixtureMessage(t, db, "answer", "main", "assistant", "claude-sonnet-4-5",
		`[{"type":"text","data":{"text":"Done"}},{"type":"finish","data":{"reason":"end_turn","time":1790856004}}]`,
		at+4, false)

	hh, err := (Crush{}).Parse(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, hh)
	assert.Equal(t, len([]rune("Fix café 世界")), updateTotals(hh).prompt)
	assert.Zero(t, updateTotals(hh).input)
	assert.Zero(t, updateTotals(hh).output)

	for _, h := range hh {
		assert.Equal(t, project, h.ProjectPathOverride)
		assert.Equal(t, "main", h.AISession)
		assert.Contains(t, h.UserAgent, "Crush")
		assert.NotEqual(t, heartbeat.FileType, h.EntityType)
		assert.NotEqual(t, float64(at+1), h.Time)
		assert.NotEqual(t, float64(at+2), h.Time)
		assert.NotEqual(t, float64(at+3), h.Time)
	}
}

func TestCrushParseSuccessfulAndFailedTools(t *testing.T) {
	home, global := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	crushFixtureRegistry(t, global, crushFixtureProject(project, data))
	db := crushFixtureDB(t, data)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()
	crushFixtureSession(t, db, "tools", "", at)

	cases := []struct {
		id, tool, path, content, metadata string
		input                             map[string]any
		failed, write                     bool
		changes                           int
	}{
		{"read", "view", "main.go", "package main", "", map[string]any{"file_path": "main.go"}, false, false, 0},
		{"write", "write", "new.go", "File written successfully", `{"additions":3,"removals":0}`,
			map[string]any{"file_path": "new.go", "content": "package main\n\nfunc main() {}\n"}, false, true, 3},
		{"edit", "edit", "edit.go", "File edited successfully", `{"additions":2,"removals":1}`,
			map[string]any{"file_path": "edit.go", "old_string": "old", "new_string": "new\nline"}, false, true, 3},
		{"multi", "multiedit", "multi.go", "Applied 2 edits", `{"additions":2,"removals":2,"edits_applied":2}`,
			map[string]any{"file_path": "multi.go", "edits": []map[string]any{
				{"old_string": "one", "new_string": "two"}, {"old_string": "three", "new_string": "four"},
			}}, false, true, 4},
		{"denied", "write", "denied.go", "Permission denied", "",
			map[string]any{"file_path": "denied.go", "content": "never written"}, true, true, 0},
		{"failed", "edit", "failed.go", "old_string not found", "",
			map[string]any{"file_path": "failed.go", "old_string": "missing", "new_string": "new"}, true, true, 0},
		{"missing", "view", "missing.go", "file not found", "",
			map[string]any{"file_path": "missing.go"}, true, false, 0},
	}

	for i, tc := range cases {
		crushFixtureMessage(t, db, tc.id+"-call", "tools", "assistant", "claude-sonnet-4-5",
			crushFixtureParts(t, crushFixtureCall(t, tc.id, tc.tool, tc.input)), at+int64(i*2), false)
		crushFixtureMessage(t, db, tc.id+"-result", "tools", "tool", "",
			crushFixtureParts(t, crushFixtureResult(tc.id, tc.tool, tc.content, tc.metadata, tc.failed)),
			at+int64(i*2+1), false)
	}

	crushFixtureMessage(t, db, "pending", "tools", "assistant", "claude-sonnet-4-5",
		crushFixtureParts(t, crushFixtureCall(t, "pending", "write",
			map[string]any{"file_path": "pending.go", "content": "not completed"})), at+20, false)

	hh, err := (Crush{}).Parse(t.Context())
	require.NoError(t, err)

	files := make(map[string]heartbeat.Heartbeat)

	for _, h := range hh {
		if h.EntityType == heartbeat.FileType {
			_, duplicate := files[h.Entity]
			assert.False(t, duplicate, "duplicate file heartbeat for %s", h.Entity)
			files[h.Entity] = h
		}
	}

	require.Len(t, files, 4)

	for _, tc := range cases[:4] {
		h, ok := files[filepath.ToSlash(filepath.Join(project, tc.path))]
		require.True(t, ok, "missing successful %s heartbeat", tc.tool)
		assert.Equal(t, heartbeat.PointerTo(tc.write), h.IsWrite)
		assert.Equal(t, "tools", h.AISession)
		assert.Contains(t, h.UserAgent, "sonnet/4-5")

		if tc.write {
			assert.Equal(t, heartbeat.PointerTo(tc.changes), h.AILineChanges)
		}
	}
}

func TestCrushParseModelSwitchAndSubagent(t *testing.T) {
	home, global := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	crushFixtureRegistry(t, global, crushFixtureProject(project, data))
	db := crushFixtureDB(t, data)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()
	crushFixtureSession(t, db, "parent", "", at)
	crushFixtureSession(t, db, "child", "parent", at)

	for i, model := range []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5"} {
		session := "parent"
		if i == 2 {
			session = "child"
		}

		crushFixtureMessage(t, db, model+"-call", session, "assistant", model,
			crushFixtureParts(t, crushFixtureCall(t, model, "view",
				map[string]any{"file_path": model + ".go"})), at+int64(i*2), false)
		crushFixtureMessage(t, db, model+"-result", session, "tool", "",
			crushFixtureParts(t, crushFixtureResult(model, "view", "package main", "", false)),
			at+int64(i*2+1), false)
	}

	crushFixtureMessage(t, db, "child-prompt", "child", "user", "",
		`[{"type":"text","data":{"text":"Generated subagent instructions"}}]`, at+2, false)
	crushFixtureMessage(t, db, "child-answer", "child", "assistant", "claude-haiku-4-5",
		`[{"type":"text","data":{"text":"Subagent answer"}}]`, at+3, false)

	hh, err := (Crush{}).Parse(t.Context())
	require.NoError(t, err)
	assert.Zero(t, updateTotals(hh).prompt)

	models := map[string]bool{}

	for _, h := range hh {
		assert.Contains(t, []string{"parent", "child"}, h.AISession)

		for _, model := range []string{"sonnet/4-5", "opus/4-1", "haiku/4-5"} {
			if assert.NotEmpty(t, h.UserAgent) && strings.Contains(h.UserAgent, model) {
				models[model] = true
			}
		}
	}

	assert.Len(t, models, 3)
}

func TestCrushParseCutoffAndIncrementalUpdate(t *testing.T) {
	home, global := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	crushFixtureRegistry(t, global, crushFixtureProject(project, data))
	db := crushFixtureDB(t, data)
	after := time.Now().Add(-time.Minute).Truncate(time.Second)
	at := after.Unix()
	crushFixtureSession(t, db, "main", "", at-100)
	crushFixtureMessage(t, db, "old", "main", "user", "",
		`[{"type":"text","data":{"text":"old prompt"}}]`, at-10, false)
	crushFixtureMessage(t, db, "new", "main", "user", "",
		`[{"type":"text","data":{"text":"new prompt"}}]`, at+1, false)
	call := crushFixtureCall(t, "streaming", "write", map[string]any{"file_path": "stream.go", "content": "one\ntwo\n"})
	crushFixtureMessage(t, db, "stream", "main", "assistant", "claude-sonnet-4-5",
		crushFixtureParts(t, call), at+2, false)

	hh, err := (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)
	assert.Equal(t, len("new prompt"), updateTotals(hh).prompt)

	for _, h := range hh {
		assert.Greater(t, h.Time, float64(at))
		assert.NotEqual(t, heartbeat.FileType, h.EntityType)
	}

	hh, err = (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)
	assert.Empty(t, hh)

	crushFixtureMessage(t, db, "completed", "main", "tool", "",
		crushFixtureParts(t, crushFixtureResult("streaming", "write", "File written successfully",
			`{"additions":2,"removals":0}`, false)), at+3, false)
	updateSQL(t, db, `UPDATE messages SET parts=?,updated_at=? WHERE id='stream'`,
		crushFixtureParts(t, call, crushFixturePart("finish", map[string]any{"reason": "tool_use", "time": at + 3})), at+3)

	hh, err = (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)
	assert.Zero(t, updateTotals(hh).prompt)

	var files []heartbeat.Heartbeat

	for _, h := range hh {
		if h.EntityType == heartbeat.FileType {
			files = append(files, h)
		}
	}

	require.Len(t, files, 1)
	assert.Equal(t, filepath.ToSlash(filepath.Join(project, "stream.go")), files[0].Entity)
	assert.Equal(t, heartbeat.PointerTo(2), files[0].AILineChanges)

	hh, err = (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)
	assert.Empty(t, hh)
}

func crushFixtureFiles(hh Heartbeats) Heartbeats {
	var files Heartbeats

	for _, h := range hh {
		if h.EntityType == heartbeat.FileType {
			files = append(files, h)
		}
	}

	return files
}

func TestCrushParseAppendedToolResultsDoNotReplay(t *testing.T) {
	home, global := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	crushFixtureRegistry(t, global, crushFixtureProject(project, data))
	db := crushFixtureDB(t, data)
	after := time.Now().Add(-time.Minute).Truncate(time.Second)
	at := after.Unix()
	crushFixtureSession(t, db, "stream", "", at)

	first := crushFixtureResult("first", "write", "File written successfully", `{"additions":1,"removals":0}`, false)
	second := crushFixtureResult("second", "write", "File written successfully", `{"additions":2,"removals":0}`, false)

	crushFixtureMessage(t, db, "calls", "stream", "assistant", "claude-sonnet-4-5",
		crushFixtureParts(t,
			crushFixtureCall(t, "first", "write", map[string]any{"file_path": "first.go", "content": "one\n"}),
			crushFixtureCall(t, "second", "write", map[string]any{"file_path": "second.go", "content": "one\ntwo\n"})),
		at+1, false)
	crushFixtureMessage(t, db, "results", "stream", "tool", "", crushFixtureParts(t, first), at+2, false)

	hh, err := (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)

	files := crushFixtureFiles(hh)
	require.Len(t, files, 1)
	assert.Equal(t, filepath.ToSlash(filepath.Join(project, "first.go")), files[0].Entity)

	updateSQL(t, db, `UPDATE messages SET parts=?,updated_at=? WHERE id='results'`,
		crushFixtureParts(t, first, second), at+3)
	hh, err = (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)

	files = crushFixtureFiles(hh)
	require.Len(t, files, 1)
	assert.Equal(t, filepath.ToSlash(filepath.Join(project, "second.go")), files[0].Entity)
	assert.Equal(t, heartbeat.PointerTo(2), files[0].AILineChanges)

	updateSQL(t, db, `UPDATE messages SET updated_at=? WHERE id='results'`, at+4)
	hh, err = (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)
	assert.Empty(t, hh)

	hh, err = (Crush{After: after}).Parse(t.Context())
	require.NoError(t, err)
	assert.Empty(t, hh)
}

func TestCrushParseAdvancedTools(t *testing.T) {
	home, global := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	crushFixtureRegistry(t, global, crushFixtureProject(project, data))
	db := crushFixtureDB(t, data)
	at := time.Now().Add(-time.Minute).Unix()
	crushFixtureSession(t, db, "advanced", "", at)
	replace := crushFixtureCall(t, "replace", "lsp_replace_symbol", map[string]any{
		"file_path": "replace.go", "symbol": "main", "replacement": "func main() {}", "action": "replace",
	})
	download := crushFixtureCall(t, "download", "download", map[string]any{
		"file_path": "download.bin", "url": "https://example.invalid/data.bin",
	})
	rename := crushFixtureCall(t, "rename", "lsp_rename", map[string]any{
		"path": project, "symbol": "Before", "new_name": "After",
	})
	crushFixtureMessage(t, db, "calls", "advanced", "assistant", "claude-sonnet-4-5",
		crushFixtureParts(t, replace, download, rename), at, false)

	metadata := `{"file_path":"replace.go","old_content":"func main() {\n old()\n}",` +
		`"new_content":"func main() {}","action":"replace"}`
	crushFixtureMessage(t, db, "results", "advanced", "tool", "", crushFixtureParts(t,
		crushFixtureResult("replace", "lsp_replace_symbol", "Replaced symbol main", metadata, false),
		crushFixtureResult("download", "download", "Downloaded 42 bytes", "", false),
		crushFixtureResult("rename", "lsp_rename", "Renamed 'Before' to 'After' in 2 file(s):\n\n  "+
			filepath.Join(project, "rename.go")+"\n  "+filepath.Join(project, "usage.go")+"\n", "", false)), at+1, false)

	hh, err := (Crush{}).Parse(t.Context())
	require.NoError(t, err)

	files := map[string]heartbeat.Heartbeat{}

	for _, h := range hh {
		if h.EntityType == heartbeat.FileType {
			assert.Equal(t, heartbeat.PointerTo(true), h.IsWrite)
			files[h.Entity] = h
		}
	}

	require.Len(t, files, 4)

	for _, name := range []string{"replace.go", "download.bin", "rename.go", "usage.go"} {
		assert.Contains(t, files, filepath.ToSlash(filepath.Join(project, name)))
	}
}

func TestCrushParsePromptModelsFollowAssistant(t *testing.T) {
	home, global := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	crushFixtureRegistry(t, global, crushFixtureProject(project, data))
	db := crushFixtureDB(t, data)
	at := time.Now().Add(-time.Minute).Unix()
	crushFixtureSession(t, db, "models", "", at)
	crushFixtureMessage(t, db, "prompt-one", "models", "user", "",
		`[{"type":"text","data":{"text":"First request"}}]`, at, false)
	crushFixtureMessage(t, db, "answer-one", "models", "assistant", "claude-sonnet-4-5",
		`[{"type":"text","data":{"text":"First answer"}}]`, at+1, false)
	crushFixtureMessage(t, db, "prompt-two", "models", "user", "",
		`[{"type":"text","data":{"text":"Second request"}}]`, at+2, false)
	crushFixtureMessage(t, db, "answer-two", "models", "assistant", "claude-opus-4-1",
		`[{"type":"text","data":{"text":"Second answer"}}]`, at+3, false)
	crushFixtureMessage(t, db, "prompt-three", "models", "user", "",
		`[{"type":"text","data":{"text":"Routed request"}}]`, at+4, false)
	crushFixtureMessage(t, db, "answer-three", "models", "assistant", "hyper-prism",
		`[{"type":"text","data":{"text":"Routed answer"}}]`, at+5, false)
	updateSQL(t, db, `UPDATE messages SET prism_model_id='claude-haiku-4-5' WHERE id='answer-three'`)

	hh, err := (Crush{}).Parse(t.Context())
	require.NoError(t, err)

	prompts := map[float64]string{}

	for _, h := range hh {
		if h.AIPromptLength > 0 {
			prompts[h.Time] = h.UserAgent
		}
	}

	require.Len(t, prompts, 3)
	assert.Contains(t, prompts[float64(at)], "sonnet/4-5")
	assert.Contains(t, prompts[float64(at+2)], "opus/4-1")
	assert.Contains(t, prompts[float64(at+4)], "haiku/4-5")
}

func TestCrushParseServerGlobalStore(t *testing.T) {
	_, global := crushFixtureHome(t)
	db := crushFixtureDB(t, filepath.Join(global, ".crush"))
	at := time.Now().Add(-time.Minute).Unix()
	crushFixtureSession(t, db, "server", "", at)
	crushFixtureMessage(t, db, "server-prompt", "server", "user", "",
		`[{"type":"text","data":{"text":"Server prompt"}}]`, at, false)

	hh, err := (Crush{}).Parse(t.Context())
	require.NoError(t, err)
	assert.Equal(t, len("Server prompt"), updateTotals(hh).prompt)
	require.NotEmpty(t, hh)
	assert.Equal(t, "server", hh[0].AISession)
}

func TestCrushParseDataDirUsesWorkingDirectory(t *testing.T) {
	home, _ := crushFixtureHome(t)
	project := filepath.Join(home, "project")
	data := filepath.Join(project, ".crush")
	db := crushFixtureDB(t, data)
	t.Chdir(project)
	t.Setenv("CRUSH_DATA_DIR", data)

	at := time.Now().Add(-time.Minute).Unix()
	crushFixtureSession(t, db, "unregistered", "", at)
	crushFixtureMessage(t, db, "unregistered-prompt", "unregistered", "user", "",
		`[{"type":"text","data":{"text":"Local project prompt"}}]`, at, false)

	hh, err := (Crush{}).Parse(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, hh)

	for _, h := range hh {
		assert.Equal(t, project, h.ProjectPathOverride)
	}
}

func TestCrushParseProjectStoreDiscovery(t *testing.T) {
	for _, mode := range []string{"global-data", "xdg-data", "data-dir"} {
		t.Run(mode, func(t *testing.T) {
			home, global := crushFixtureHome(t)

			switch mode {
			case "xdg-data":
				t.Setenv("CRUSH_GLOBAL_DATA", "")

				xdg := filepath.Join(home, "xdg")
				t.Setenv("XDG_DATA_HOME", xdg)
				global = filepath.Join(xdg, "crush")
			case "data-dir":
				t.Setenv("CRUSH_DATA_DIR", filepath.Join(home, "custom-state"))
			case "global-data":
			}

			first := filepath.Join(home, "first")
			second := filepath.Join(home, "second")
			firstData := filepath.Join(first, ".crush")
			secondData := filepath.Join(home, "custom-state")
			crushFixtureRegistry(t, global,
				crushFixtureProject(first, firstData), crushFixtureProject(first, firstData),
				crushFixtureProject(second, secondData), crushFixtureProject(filepath.Join(home, "deleted"), ""))

			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()

			for _, dir := range []string{firstData, secondData} {
				db := crushFixtureDB(t, dir)
				crushFixtureSession(t, db, "same-session-id", "", at)
				crushFixtureMessage(t, db, "same-message-id", "same-session-id", "user", "",
					`[{"type":"text","data":{"text":"hello"}}]`, at, false)
			}

			hh, err := (Crush{}).Parse(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 10, updateTotals(hh).prompt)

			projects := map[string]bool{}
			for _, h := range hh {
				projects[h.ProjectPathOverride] = true
			}

			assert.Equal(t, map[string]bool{first: true, second: true}, projects)
		})
	}
}
