package ai

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func crushSQLiteQuery(_ string, projection string, after time.Time) (string, []any) {
	projection = strings.ReplaceAll(projection, "*", `id, session_id, role, model,
 created_at, updated_at, is_summary_message,
 (SELECT parent_session_id FROM sessions WHERE id = messages.session_id) AS crush_parent,
 (SELECT json_group_array(json_object(
 'type', json_extract(part.value, '$.type'),
 'data', CASE json_extract(part.value, '$.type')
 WHEN 'text' THEN json_object('text', json_extract(part.value, '$.data.text'),
 'hidden', json(CASE WHEN json_extract(part.value, '$.data.hidden') THEN 'true' ELSE 'false' END))
 WHEN 'tool_result' THEN json_object(
 'tool_call_id', json_extract(part.value, '$.data.tool_call_id'),
 'name', json_extract(part.value, '$.data.name'),
 'content', CASE WHEN json_extract(part.value, '$.data.name') = 'lsp_rename'
 THEN json_extract(part.value, '$.data.content') END,
 'is_error', json(CASE WHEN json_extract(part.value, '$.data.is_error') THEN 'true' ELSE 'false' END),
 'metadata', CASE WHEN json_valid(json_extract(part.value, '$.data.metadata')) THEN json_object(
 'additions', json_extract(json_extract(part.value, '$.data.metadata'), '$.additions'),
 'removals', json_extract(json_extract(part.value, '$.data.metadata'), '$.removals')) END,
 'call', json((SELECT json_object(
 'name', json_extract(assistant.call_data, '$.name'),
 'input', CASE WHEN json_valid(json_extract(assistant.call_data, '$.input')) THEN json_object(
 'file_path', json_extract(json_extract(assistant.call_data, '$.input'), '$.file_path')) END,
 'finished', json(CASE WHEN json_extract(assistant.call_data, '$.finished') THEN 'true' ELSE 'false' END),
 'model', assistant.model)
 FROM crush_calls AS assistant
 WHERE assistant.session_id = messages.session_id
 AND assistant.call_id = json_extract(part.value, '$.data.tool_call_id')
 LIMIT 1)))
 END)) FROM json_each(CASE WHEN json_valid(parts) THEN parts ELSE '[]' END) AS part
 WHERE (messages.role = 'user' AND json_extract(part.value, '$.type') = 'text')
 OR (messages.role = 'tool' AND json_extract(part.value, '$.type') = 'tool_result')) AS parts`)

	return `WITH crush_calls AS MATERIALIZED (
 SELECT session_id, model, json_extract(call.value, '$.data') AS call_data,
 json_extract(call.value, '$.data.id') AS call_id
 FROM messages, json_each(CASE WHEN json_valid(parts) THEN parts ELSE '[]' END) AS call
 WHERE role = 'assistant' AND json_extract(call.value, '$.type') = 'tool_call'
 ) SELECT ` + projection + ` FROM messages WHERE
 NOT (role = 'user' AND EXISTS (SELECT 1 FROM json_each(
 CASE WHEN json_valid(parts) THEN parts ELSE '[]' END) AS excluded
 WHERE json_extract(excluded.value, '$.type') IN ('shell_command', 'sub_agent_report')))
 AND (role = 'user' OR COALESCE(NULLIF(updated_at, 0), created_at) >= ?)`, []any{after.Unix()}
}

type crushPart struct {
	Type string `json:"type"`
	Data struct {
		Content    string          `json:"content"`
		Text       string          `json:"text"`
		Hidden     bool            `json:"hidden"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
		IsError    bool            `json:"is_error"`
		Metadata   json.RawMessage `json:"metadata"`
		Call       *crushCall      `json:"call"`
	} `json:"data"`
}

type crushCall struct {
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Finished bool            `json:"finished"`
	Model    string          `json:"model"`
}

func crushDecode(value any, target any) bool {
	var raw []byte

	switch value := value.(type) {
	case string:
		raw = []byte(value)
	case []byte:
		raw = value
	case json.RawMessage:
		raw = value
	default:
		var err error

		raw, err = json.Marshal(value)
		if err != nil {
			return false
		}
	}

	var text string
	if json.Unmarshal(raw, &text) == nil {
		raw = []byte(text)
	}

	return json.Unmarshal(raw, target) == nil
}

func crushSQLiteEvents(table string, row map[string]any) []genericAIEvent {
	if table != "messages" {
		return nil
	}

	var parts []crushPart
	if !crushDecode(row["parts"], &parts) {
		return nil
	}

	event := genericAIEvent{
		sessionID: genericAIString(row["session_id"]), model: genericAIString(row["model"]),
		timestamp: genericAITime(row["updated_at"]),
	}
	if genericAIInt64(row["updated_at"]) == 0 || event.timestamp.IsZero() {
		event.timestamp = genericAITime(row["created_at"])
	}

	role := genericAIString(row["role"])
	if role == "user" {
		if genericAIString(row["crush_parent"]) != "" || genericAIInt64(row["is_summary_message"]) != 0 {
			return nil
		}

		var texts []string

		for _, part := range parts {
			if part.Type == "text" && !part.Data.Hidden {
				texts = append(texts, part.Data.Text)
			}
		}

		event.recordID = "prompt:" + genericAIString(row["id"])
		event.prompt = strings.TrimSpace(strings.Join(texts, "\n"))

		return []genericAIEvent{event}
	}

	if role != "tool" {
		return nil
	}

	var events []genericAIEvent

	for _, part := range parts {
		if part.Type != "tool_result" || part.Data.IsError || part.Data.Call == nil || !part.Data.Call.Finished {
			continue
		}

		call := part.Data.Call
		if part.Data.Name != "" && part.Data.Name != call.Name {
			continue
		}

		tool := event
		tool.recordID = "tool:" + part.Data.ToolCallID
		tool.model = firstNonEmptyString(call.Model, tool.model)
		tool.toolName = call.Name

		if call.Name == "lsp_rename" {
			for _, path := range crushRenameFiles(part.Data.Content) {
				renamed := tool
				renamed.filePath = path
				renamed.recordID += ":" + path
				renamed.isWrite = true
				events = append(events, renamed)
			}

			continue
		}

		var input struct {
			FilePath string `json:"file_path"`
		}
		if !crushDecode(call.Input, &input) {
			continue
		}

		tool.filePath = input.FilePath

		switch call.Name {
		case "view":
		case "write", "edit", "multiedit", "lsp_replace_symbol", "download":
			tool.isWrite = true
		default:
			continue
		}

		if tool.filePath == "" {
			continue
		}

		if tool.isWrite {
			tool.lineChanges = crushLineChanges(part.Data.Metadata)
		}

		events = append(events, tool)
	}

	return events
}

func crushLineChanges(raw json.RawMessage) *int {
	var metadata struct {
		Additions *int `json:"additions"`
		Removals  *int `json:"removals"`
	}
	if !crushDecode(raw, &metadata) || metadata.Additions == nil || metadata.Removals == nil {
		return nil
	}

	changes := max(*metadata.Additions, 0) + max(*metadata.Removals, 0)

	return &changes
}

var crushRenameHeader = regexp.MustCompile(`^Renamed '.+' to '.+' in ([0-9]+) file\(s\):$`)

func crushRenameFiles(content string) []string {
	sections := strings.SplitN(content, "\n\n", 3)
	if len(sections) < 2 {
		return nil
	}

	match := crushRenameHeader.FindStringSubmatch(sections[0])
	if match == nil {
		return nil
	}

	count, err := strconv.Atoi(match[1])
	if err != nil || count < 1 {
		return nil
	}

	lines := strings.Split(strings.TrimSuffix(sections[1], "\n"), "\n")
	if len(lines) != count {
		return nil
	}

	paths := make([]string, 0, count)
	seen := make(map[string]bool)

	for _, line := range lines {
		if !strings.HasPrefix(line, "  ") {
			return nil
		}

		path := strings.TrimPrefix(line, "  ")
		if !genericAIPathIsAbs(path) || seen[path] {
			return nil
		}

		seen[path] = true
		paths = append(paths, path)
	}

	return paths
}
