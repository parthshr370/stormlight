// Package progress manages the anti-stateless checkpoint: on each todo_write,
// it atomically rewrites .harness/progress.md (tmp+rename, mutex-guarded) so a
// crash mid-build doesn't lose the working state. Validate rejects control
// characters that would break the single-line checklist format.
package progress

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// TodoItem is a single todo with id, content, status, and optional priority.
type TodoItem struct {
	ID       string `json:"id,omitempty"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

// Store is a thread-safe progress store backed by .harness/progress.md.
// Update atomically rewrites the file (tmp+rename, mutex-guarded) so a
// crash mid-build doesn't lose working state.
type Store struct {
	mu    sync.Mutex
	cwd   string
	todos []TodoItem
}

// NewStore starts a progress store rooted at cwd.
func NewStore(cwd string) *Store {
	return &Store{cwd: cwd}
}

// Update validates todos, renders them to markdown, and atomically writes
// .harness/progress.md. Returns validation errors without touching the file.
func (s *Store) Update(ctx context.Context, todos []TodoItem) error {
	if err := Validate(todos); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(todos) == 0 {
		return nil
	}
	copyTodos := cloneTodos(todos)
	content := RenderMarkdown(copyTodos)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(s.cwd, ".harness"), 0o755); err != nil {
		return err
	}
	progressPath := filepath.Join(s.cwd, ".harness", "progress.md")
	tmpPath := progressPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, progressPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	s.todos = copyTodos
	return nil
}

// Todos returns a copy of the current todos.
func (s *Store) Todos() []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTodos(s.todos)
}

// Validate checks that every todo has non-empty content, printable
// characters, and valid status/priority values.
func Validate(todos []TodoItem) error {
	for i, todo := range todos {
		if strings.TrimSpace(todo.Content) == "" {
			return fmt.Errorf("todo %d content is required", i+1)
		}
		for _, r := range todo.Content {
			if unicode.IsControl(r) {
				return fmt.Errorf("todo %d content must be a single printable line", i+1)
			}
		}
		switch todo.Status {
		case "pending", "in_progress", "completed":
		default:
			return fmt.Errorf("todo %d has invalid status %q", i+1, todo.Status)
		}
		if todo.Priority != "" {
			switch todo.Priority {
			case "high", "medium", "low":
			default:
				return fmt.Errorf("todo %d has invalid priority %q", i+1, todo.Priority)
			}
		}
	}
	return nil
}

// RenderMarkdown renders todos as a markdown checklist with status and
// priority annotations.
func RenderMarkdown(todos []TodoItem) string {
	var b strings.Builder
	b.WriteString("# Progress\n\n")
	if len(todos) == 0 {
		b.WriteString("No todos.\n")
		return b.String()
	}
	for _, todo := range todos {
		box := "[ ]"
		if todo.Status == "completed" {
			box = "[x]"
		}
		b.WriteString("- ")
		b.WriteString(box)
		b.WriteByte(' ')
		if todo.ID != "" {
			b.WriteString("`")
			b.WriteString(escapeBackticks(todo.ID))
			b.WriteString("` ")
		}
		b.WriteString(todo.Content)
		metadata := []string{}
		if todo.Status == "in_progress" {
			metadata = append(metadata, "in progress")
		}
		if todo.Priority != "" {
			metadata = append(metadata, todo.Priority+" priority")
		}
		if len(metadata) > 0 {
			b.WriteString(" _(")
			b.WriteString(strings.Join(metadata, ", "))
			b.WriteString(")_")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ProgressPath returns the path to .harness/progress.md under cwd.
func ProgressPath(cwd string) string {
	if cwd == "" {
		return filepath.Join(".harness", "progress.md")
	}
	return filepath.Join(cwd, ".harness", "progress.md")
}

func cloneTodos(todos []TodoItem) []TodoItem {
	if todos == nil {
		return nil
	}
	out := make([]TodoItem, len(todos))
	copy(out, todos)
	return out
}

func escapeBackticks(s string) string {
	return strings.ReplaceAll(s, "`", "\\`")
}
