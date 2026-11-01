package webhook

import (
	"github.com/jozala/omnigrex/internal/collaboration"
	"strings"
)

// ParseCollaborationCommand recognizes only an entire first nonblank line in a
// newly created Issue comment. Everything after that line is exact free-form
// content, not another executable command. Edits and deletes are inert.
func ParseCollaborationCommand(event, action, body string) (collaboration.Command, bool) {
	if event != "issue_comment" || action != "created" {
		return collaboration.Command{}, false
	}
	lines := strings.SplitAfter(body, "\n")
	for i, line := range lines {
		first := strings.TrimSpace(line)
		if first == "" {
			continue
		}
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			return collaboration.Command{}, false
		}
		if !strings.HasPrefix(first, "/omnigrex ") {
			return collaboration.Command{}, false
		}
		fields := strings.Fields(first)
		command := collaboration.Command{Kind: "invalid", Body: strings.Join(lines[i+1:], "")}
		if len(fields) == 2 && fields[1] == "steer" {
			command.Kind = "steer"
		}
		if len(fields) == 3 && (fields[1] == "answer" || fields[1] == "amend") {
			command.Kind = fields[1]
			command.Target = fields[2]
		}
		return command, true
	}
	return collaboration.Command{}, false
}
