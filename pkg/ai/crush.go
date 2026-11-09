package ai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nghiant03/wakatime-cli/pkg/log"
)

// Crush contains parameters for detecting heartbeats from Crush logs.
type Crush ParserConfig

// Parse parses Crush logs for AI heartbeats.
func (g Crush) Parse(ctx context.Context) (Heartbeats, error) {
	home, err := aiUserHome(ctx)
	if err != nil {
		return nil, err
	}

	stores := crushStores(ctx, home, g.ProjectInfo.ProjectPathOverride)

	var result Heartbeats

	for _, store := range stores {
		provider := g.sqliteProvider(store.path, store.cwd)
		crushPrepareProvider(ctx, &provider, store.path)

		parsed, err := parseGenericAIProvider(ctx, provider)
		for i := range parsed {
			if parsed[i].ProjectPathOverride == "" {
				parsed[i].ProjectPathOverride = store.cwd
			}
		}

		result = append(result, parsed...)
		if err != nil {
			return result, err
		}
	}

	return result, nil
}

// Name returns the Crush parser name.
func (Crush) Name() string { return "Crush" }

type crushStore struct{ path, cwd string }

func crushGlobalData(home string) string {
	fallback := filepath.Join(home, ".local", "share", "crush")
	if runtime.GOOS == "windows" {
		fallback = filepath.Join(envOrDefault("LOCALAPPDATA", filepath.Join(home, "AppData", "Local")), "crush")
	}

	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		fallback = filepath.Join(xdg, "crush")
	}

	return envOrDefault("CRUSH_GLOBAL_DATA", fallback)
}

func crushStores(ctx context.Context, home, project string) []crushStore {
	var stores []crushStore

	seen := make(map[string]bool)
	add := func(dir, cwd string) {
		if dir == "" {
			return
		}

		path, err := filepath.Abs(filepath.Join(dir, "crush.db"))
		if err != nil {
			return
		}

		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}

		if seen[path] {
			return
		}

		seen[path] = true
		stores = append(stores, crushStore{path: path, cwd: cwd})
	}
	global := crushGlobalData(home)

	var registry struct {
		Projects []struct {
			Path    string `json:"path"`
			DataDir string `json:"data_dir"`
		} `json:"projects"`
	}

	raw, err := os.ReadFile(filepath.Clean(filepath.Join(global, "projects.json")))
	if err == nil {
		err = json.Unmarshal(raw, &registry)
	}

	if err != nil && !os.IsNotExist(err) {
		log.Extract(ctx).Warnf("failed reading Crush project registry: %s", err)
	}

	for _, registered := range registry.Projects {
		dir := registered.DataDir
		if dir == "" {
			dir = filepath.Join(registered.Path, ".crush")
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(registered.Path, dir)
		}

		add(dir, registered.Path)
	}

	if override := strings.TrimSpace(os.Getenv("CRUSH_DATA_DIR")); override != "" {
		cwd := project
		if cwd == "" {
			if workingDir, err := os.Getwd(); err == nil {
				cwd = workingDir
			}
		}

		add(override, cwd)
	}

	for dir := project; dir != ""; dir = filepath.Dir(dir) {
		add(filepath.Join(dir, ".crush"), dir)

		if filepath.Dir(dir) == dir {
			break
		}
	}

	add(filepath.Join(global, ".crush"), global)
	add(global, global)

	return stores
}

func (g Crush) sqliteProvider(path, cwd string) genericAIProvider {
	return genericAIProvider{
		parser: g, config: ParserConfig(g), sqliteRoots: []string{path},
		sqliteTables: []string{"messages"}, sqliteQuery: crushSQLiteQuery,
		sqliteEvents: func(table string, row map[string]any) []genericAIEvent {
			events := crushSQLiteEvents(table, row)
			for i := range events {
				events[i].cwd = cwd
			}

			return events
		}, sqliteParserVersion: 1,
	}
}
