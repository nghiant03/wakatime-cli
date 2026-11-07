package ai

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nghiant03/wakatime-cli/pkg/ini"
)

func crushPrepareProvider(ctx context.Context, provider *genericAIProvider, path string) {
	columns := crushMessageColumns(ctx, path)
	provider.sqliteQuery = func(table, projection string, after time.Time) (string, []any) {
		query, args := crushSQLiteQuery(table, projection, after)
		if !columns["is_summary_message"] {
			query = strings.ReplaceAll(query, "updated_at, is_summary_message,", "updated_at, 0 AS is_summary_message,")
		}

		model := crushModelSQL(columns, "assistant")
		query = strings.ReplaceAll(query, "SELECT session_id, model,",
			"SELECT session_id, "+strings.ReplaceAll(model, "assistant.", "messages.")+" AS model,")
		query = strings.Replace(query, "role, model,", `role, CASE WHEN role = 'user' THEN
 (SELECT `+model+` FROM messages AS assistant WHERE assistant.session_id = messages.session_id
 AND assistant.rowid > messages.rowid AND assistant.role IN ('assistant', 'user')
 ORDER BY assistant.rowid LIMIT 1) ELSE model END AS model,`, 1)

		return query, args
	}
	seen := crushObservedTools(ctx, path, provider.config.After)
	original := provider.sqliteEvents
	provider.sqliteEvents = func(table string, row map[string]any) []genericAIEvent {
		events := original(table, row)

		result := events[:0]
		for _, event := range events {
			if event.filePath != "" {
				if seen[event.recordID] {
					continue
				}

				seen[event.recordID] = true
			}

			result = append(result, event)
		}

		return result
	}
}

func crushMessageColumns(ctx context.Context, path string) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	columns := make(map[string]bool)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}

	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return columns
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('messages')")
	if err != nil {
		return columns
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			columns[name] = true
		}
	}

	return columns
}

func crushModelSQL(columns map[string]bool, alias string) string {
	var values []string

	for _, column := range []string{"prism_model_id", "prism_model_name", "model", "model_name"} {
		if columns[column] {
			values = append(values, "NULLIF("+alias+"."+column+", '')")
		}
	}

	values = append(values, "''")

	return "COALESCE(" + strings.Join(values, ", ") + ")"
}

func crushObservedTools(ctx context.Context, path string, after time.Time) map[string]bool {
	seen := make(map[string]bool)

	dir, err := ini.WakaResourcesDir(ctx)
	if err != nil {
		return seen
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return seen
	}

	key := sha256.Sum256([]byte("Crush\x00" + absolute + "\x00messages"))

	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "ai-sqlite", fmt.Sprintf("%x.json", key))))
	if err != nil {
		return seen
	}

	var cursor struct {
		Cutoff time.Time
		State  struct{ Records map[string]json.RawMessage }
	}
	if json.Unmarshal(raw, &cursor) != nil || after.Before(cursor.Cutoff) {
		return seen
	}

	for id := range cursor.State.Records {
		if strings.HasPrefix(id, "tool:") {
			seen[id] = true
		}
	}

	return seen
}
