package documentation_test

import (
	"fmt"
	"html"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode"
)

// Check the repository's public Markdown, without fetching external sites.
// Fenced examples and inline code are not links. References and explicit HTML
// anchors are supported alongside the inline links used by the audience guides.
func TestDocumentationLinks(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate documentation test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		for _, err := range checkLinks(root, path) {
			t.Error(err)
		}
	}
}

var (
	inlineLink   = regexp.MustCompile(`!?\[([^\]\n]*)\]\((<[^>\n]*>|[^\s)]+)(?:\s+"[^"]*")?\)`)
	reference    = regexp.MustCompile(`^ {0,3}\[([^\]]+)\]:\s*(<[^>]*>|\S+)`)
	referenceUse = regexp.MustCompile(`!?\[([^\]\n]+)\]\[([^\]\n]*)\]`)
	inlineCode   = regexp.MustCompile("`+[^`]*`+")
	heading      = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.+?)(?:\s+#+\s*)?$`)
	setext       = regexp.MustCompile(`^ {0,3}(=+|-+)\s*$`)
	htmlAnchor   = regexp.MustCompile(`(?i)<(?:a|h[1-6])\b[^>]*\b(?:id|name)=["']([^"']+)["']`)
)

type markdownLink struct {
	line   int
	target string
}

func markdown(content string) (map[string]bool, []markdownLink) {
	anchors := make(map[string]bool)
	var links []markdownLink
	definitions := make(map[string]string)
	type pendingReference struct {
		name   string
		number int
	}
	var pending []pendingReference
	var fence byte
	var fenceSize int
	previous := ""
	addHeading := func(text string) {
		text = inlineLink.ReplaceAllString(text, "$1")
		text = html.UnescapeString(strings.ToLower(strings.ReplaceAll(text, "`", "")))
		var slug strings.Builder
		for _, character := range text {
			switch {
			case character == ' ':
				slug.WriteByte('-')
			case character == '-' || character == '_' || !unicode.IsPunct(character) && !unicode.IsSymbol(character):
				slug.WriteRune(character)
			}
		}
		base, candidate := slug.String(), slug.String()
		for suffix := 1; anchors[candidate]; suffix++ {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		anchors[candidate] = true
	}
	for index, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			size := len(trimmed) - len(strings.TrimLeft(trimmed, string(trimmed[0])))
			if size >= 3 {
				if fence == 0 {
					fence, fenceSize = trimmed[0], size
				} else if trimmed[0] == fence && size >= fenceSize && strings.TrimSpace(trimmed[size:]) == "" {
					fence = 0
				}
				previous = ""
				continue
			}
		}
		if fence != 0 {
			continue
		}
		if match := heading.FindStringSubmatch(line); match != nil {
			addHeading(match[1])
		} else if previous != "" && setext.MatchString(line) {
			addHeading(previous)
		}
		for _, match := range htmlAnchor.FindAllStringSubmatch(line, -1) {
			anchors[match[1]] = true
		}
		text := inlineCode.ReplaceAllString(line, "")
		if match := reference.FindStringSubmatch(text); match != nil {
			definitions[strings.ToLower(match[1])] = strings.Trim(match[2], "<>")
		} else {
			for _, match := range inlineLink.FindAllStringSubmatch(text, -1) {
				links = append(links, markdownLink{index + 1, strings.Trim(match[2], "<>")})
			}
			for _, match := range referenceUse.FindAllStringSubmatch(text, -1) {
				name := match[2]
				if name == "" {
					name = match[1]
				}
				pending = append(pending, pendingReference{strings.ToLower(name), index + 1})
			}
		}
		previous = strings.TrimSpace(line)
	}
	for _, use := range pending {
		target, found := definitions[use.name]
		if !found {
			// An unresolved reference is not a rendered Markdown link.
			continue
		}
		links = append(links, markdownLink{use.number, target})
	}
	return anchors, links
}

func checkLinks(root, path string) []error {
	content, err := os.ReadFile(path)
	if err != nil {
		return []error{err}
	}
	_, links := markdown(string(content))
	var problems []error
	for _, link := range links {
		if err := checkTarget(root, path, link.target); err != nil {
			name, _ := filepath.Rel(root, path)
			problems = append(problems, fmt.Errorf("%s:%d: %s: %w", name, link.line, link.target, err))
		}
	}
	return problems
}

func checkTarget(root, source, target string) error {
	parsed, err := url.Parse(target)
	if err != nil {
		return err
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		return nil
	}
	path := source
	if parsed.Path != "" {
		if strings.HasPrefix(parsed.Path, "/") {
			path = filepath.Join(root, filepath.FromSlash(parsed.Path))
		} else {
			path = filepath.Join(filepath.Dir(source), filepath.FromSlash(parsed.Path))
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if parsed.Fragment == "" {
			return nil
		}
		path = filepath.Join(path, "README.md")
	}
	if parsed.Fragment == "" || !strings.EqualFold(filepath.Ext(path), ".md") {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	anchors, _ := markdown(string(content))
	if !anchors[parsed.Fragment] {
		return fmt.Errorf("missing anchor #%s", parsed.Fragment)
	}
	return nil
}

func TestLocalLinksAndAnchors(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"README.md":   "# Start\n[Encoded](page%20one.md#api--state)\n[Repeated](page%20one.md#repeat-1)\n[Explicit](page%20one.md#legacy)\n[Setext](page%20one.md#setext)\n[Reference][page]\n[page]: page%20one.md#repeat\n`[ignored](missing.md)`\n~~~md\n[ignored](absent.md)\n~~~\n[External](https://example.com/missing#remote)\n",
		"page one.md": "# API & `State`\n## Repeat\n## Repeat\n<a id=\"legacy\"></a>\nSetext\n======\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(root, "README.md")
	if problems := checkLinks(root, source); len(problems) != 0 {
		t.Fatalf("valid Markdown rejected: %v", problems)
	}
	for _, target := range []string{"missing.md", "page%20one.md#missing", "#absent"} {
		if err := checkTarget(root, source, target); err == nil {
			t.Errorf("broken navigation accepted: %s", target)
		}
	}
}
