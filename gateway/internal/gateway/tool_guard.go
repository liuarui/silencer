package gateway

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"mvdan.cc/sh/v3/syntax"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type toolGuardPolicy struct {
	enabled bool
	mode    string
}

func validateToolGuard(control map[string]any) error {
	if _, present := control["audit"]; !present {
		return nil
	}
	audit, ok := control["audit"].(map[string]any)
	if !ok {
		return errors.New("audit must be an object")
	}
	value, exists := audit["tool_guard"]
	if !exists {
		return nil
	}
	guard, ok := value.(map[string]any)
	if !ok {
		return errors.New("audit.tool_guard must be an object")
	}
	for k, v := range guard {
		switch k {
		case "enabled":
			if _, ok := v.(bool); !ok {
				return errors.New("tool guard enabled must be boolean")
			}
		case "mode":
			if v != "observe" && v != "rewrite" && v != "block" {
				return errors.New("tool guard mode must be observe, rewrite or block")
			}
		default:
			return fmt.Errorf("unknown tool guard setting: %s", k)
		}
	}
	return nil
}

func (s *Server) toolGuardSettings() toolGuardPolicy {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	policy := toolGuardPolicy{enabled: true, mode: "observe"}
	audit, _ := s.control["audit"].(map[string]any)
	guard, _ := audit["tool_guard"].(map[string]any)
	if enabled, ok := guard["enabled"].(bool); ok {
		policy.enabled = enabled
	}
	if mode, ok := guard["mode"].(string); ok {
		policy.mode = mode
	}
	return policy
}

type toolFinding struct{ rule, command, tool string }

// Commands are inspected only inside tool inputs, never prose or reasoning.
// RE2 patterns have bounded, linear running time. This detects explicit risky
// operations; it is not a sandbox or an interpreter for arbitrary programs.
var toolDangerPatterns = []struct {
	id, pattern string
	rx          *regexp.Regexp
}{
	{id: "recursive_delete", pattern: `(?i)(?:^|[^\w])(?:/\w[\w/.-]*/)?rm\s+[^;\n]*(?:-[a-z]*r[a-z]*\b|--recursive\b|\s["']?[/~]?\*|\s/[\s"']*(?:$|;))`},
	{id: "recursive_delete", pattern: `(?i)\b(?:find\s+[^\n;]*-delete\b|xargs\s+[^\n;]*\brm\b|(?:shutil\.)?rmtree\s*\(|remove-item\s+[^\n;]*-(?:recurse|r)\b|(?:rd|rmdir)\s+/s\b|del\s+/[sqf][^\n;]*\*)`},
	{id: "bulk_delete", pattern: `(?i)\brm\s+[^;\n]*\*`},
	{id: "disk_destroy", pattern: `(?i)(?:\bmkfs(?:\.[\w]+)?\b|\b(?:wipefs|shred)\s+|\bdd\s+[^\n;]*\bof\s*=\s*["']?/dev/|\bformat\s+[a-z]:|\bdiskpart\b)`},
	{id: "database_destroy", pattern: `(?i)\b(?:drop\s+(?:database|schema|table)|truncate\s+(?:table\s+)?[\w"` + "`" + `\[])`},
	{id: "remote_execution", pattern: `(?i)\b(?:curl|wget)\s+[^\n;]*\|\s*(?:sudo\s+)?(?:/bin/)?(?:ba|z|da|k)?sh\b|\b(?:invoke-expression|iex)\s*\(?\s*(?:iwr|irm|invoke-webrequest|invoke-restmethod)\b`},
	{id: "obscured_execution", pattern: `(?i)\b(?:powershell|pwsh)(?:\.exe)?\s+[^\n;]*-(?:enc|encodedcommand)\b|\bbase64\s+[^\n;]*\|\s*(?:ba|z)?sh\b`},
	{id: "infrastructure_destroy", pattern: `(?i)\b(?:terraform\s+(?:[^\n;]*\s)?destroy|kubectl\s+delete\s+[^\n;]*(?:--all\b|namespaces?\b)|aws\s+s3\s+(?:rb\s+[^\n;]*--force|rm\s+[^\n;]*--recursive))`},
	{id: "git_destructive", pattern: `(?i)\bgit\s+(?:reset\s+--hard|clean\s+[^\n;]*-[a-z]*[fd][a-z]*|push\s+[^\n;]*(?:--force\b|-[a-z]*f\b))`},
	{id: "fork_bomb", pattern: `:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;?\s*:`},
}

func init() {
	for i := range toolDangerPatterns {
		toolDangerPatterns[i].rx = regexp.MustCompile(toolDangerPatterns[i].pattern)
	}
}

var sqlMutation = regexp.MustCompile(`(?is)\b(?:delete\s+from|update\s+\S+\s+set)\b`)
var sqlWhere = regexp.MustCompile(`(?i)\bwhere\b`)
var sqlComments = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)
var sqlStart = regexp.MustCompile(`(?i)^\s*(?:select|with|delete|update|insert|drop|truncate|alter|create|begin|explain)\b`)
var sqlUnbounded = regexp.MustCompile(`(?i)\bwhere\s*\(?\s*(?:true\b|1\s*=\s*1\b|0\s*=\s*0\b)\s*\)?\s*(?:$|returning\b)|\bor\s+(?:true\b|1\s*=\s*1\b)\s*\)?\s*(?:$|returning\b)`)
var executionToolName = regexp.MustCompile(`(?i)(?:exec|shell|terminal|bash|python|powershell|command|script|sql|query|run|code)`)
var deletionToolName = regexp.MustCompile(`(?i)(?:delete|remove|destroy|purge|truncate|drop)`)

func inspectTool(name, args string, custom bool) []toolFinding {
	if len(args) > 2<<20 {
		return []toolFinding{{"inspection_limit", "工具参数超过安全检查上限", name}}
	}
	var value any
	if !custom && json.Unmarshal([]byte(args), &value) != nil {
		return []toolFinding{{"invalid_tool_arguments", "工具参数不完整或不是有效 JSON", name}}
	}
	if custom {
		value = args
	}
	var commands []string
	var walk func(any, string, bool)
	walk = func(v any, key string, executable bool) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				active := executable
				switch strings.ToLower(k) {
				case "command", "cmd", "commands", "script", "code", "sql", "query", "argv":
					active = true
				case "description", "explanation", "reason", "content", "file_content":
					active = false
				}
				walk(x[k], k, active)
			}
		case []any:
			if executable {
				var argv []string
				for _, item := range x {
					if s, ok := item.(string); ok {
						argv = append(argv, s)
					}
				}
				if len(argv) > 0 {
					commands = append(commands, strings.Join(argv, " "))
				}
			}
			for _, item := range x {
				walk(item, key, executable)
			}
		case string:
			if executable {
				commands = append(commands, x)
			}
		}
	}
	walk(value, "", custom || executionToolName.MatchString(name))
	var findings []toolFinding
	seen := map[string]bool{}
	add := func(rule, cmd string) {
		if !seen[rule] {
			seen[rule] = true
			findings = append(findings, toolFinding{rule, cmd, truncateGuardText(name, 80)})
		}
	}
	for _, command := range commands {
		command = strings.ReplaceAll(command, "\\\n", "")
		for _, candidate := range executableCommandTexts(command) {
			for _, pattern := range toolDangerPatterns {
				if pattern.rx.MatchString(candidate) {
					add(pattern.id, command)
				}
			}
			for _, statement := range strings.Split(sqlComments.ReplaceAllString(candidate, " "), ";") {
				if sqlMutation.MatchString(statement) && (!sqlWhere.MatchString(stripSQLLiterals(statement)) || sqlUnbounded.MatchString(stripSQLLiterals(statement))) {
					add("database_unbounded_mutation", command)
				}
			}
		}
	}
	if deletionToolName.MatchString(name) {
		lowerName := strings.ToLower(name)
		if strings.Contains(lowerName, "delete_all") || strings.Contains(lowerName, "drop_database") || strings.Contains(lowerName, "drop_table") || strings.Contains(lowerName, "truncate") || strings.Contains(lowerName, "destroy") {
			add("bulk_delete_tool", name+" "+args)
		}
		var inspectDelete func(any)
		inspectDelete = func(value any) {
			if obj, ok := value.(map[string]any); ok {
				for k, v := range obj {
					inspectDelete(v)
					key := strings.ToLower(k)
					if ((key == "all" || key == "recursive" || key == "recurse") && v == true) || ((key == "filter" || key == "where") && isEmptyGuardObject(v)) {
						add("bulk_delete_tool", name+" "+args)
					}
					if key == "path" || key == "target" || key == "scope" {
						if s, ok := v.(string); ok && (s == "/" || s == "*" || s == "all" || s == "~" || strings.HasSuffix(s, "/*")) {
							add("bulk_delete_tool", name+" "+args)
						}
					}
				}
				if strings.Contains(strings.ToLower(name), "delete_many") && len(obj) == 0 {
					add("bulk_delete_tool", name+" {}")
				}
			}
			if values, ok := value.([]any); ok {
				for _, v := range values {
					inspectDelete(v)
				}
			}
		}
		inspectDelete(value)
	}
	return findings
}

// Parse shell syntax so quoted command names are recognized and commands
// merely printed by echo/printf are ignored. Command substitutions still get
// their own AST visit. Other languages fall back to explicit pattern checks.
func executableCommandTexts(command string) []string {
	if sqlStart.MatchString(command) {
		return []string{stripSQLLiterals(command)}
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return []string{command}
	}
	var candidates []string
	remoteSource := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var words []string
		for _, word := range call.Args {
			words = append(words, literalShellParts(word.Parts))
		}
		name := strings.ToLower(words[0])
		if name == "curl" || name == "wget" || name == "base64" {
			remoteSource = true
		}
		name = strings.TrimPrefix(name, "/bin/")
		name = strings.TrimPrefix(name, "/usr/bin/")
		switch name {
		case "echo", "printf", "true", "false", "test", ":":
			return true
		}
		if name == "git" && len(words) > 1 && words[1] != "push" && words[1] != "reset" && words[1] != "clean" {
			return true
		}
		candidates = append(candidates, strings.Join(words, " "))
		return true
	})
	// Pipelines have their operators outside CallExpr; inspect just that wire
	// shape for remote execution while excluding ordinary quoted output above.
	for _, pattern := range toolDangerPatterns {
		if (pattern.id == "remote_execution" || pattern.id == "obscured_execution") && remoteSource && pattern.rx.MatchString(command) {
			candidates = append(candidates, command)
			break
		}
	}
	if len(candidates) == 0 && !strings.HasPrefix(strings.TrimSpace(command), "echo ") && !strings.HasPrefix(strings.TrimSpace(command), "printf ") {
		for _, p := range toolDangerPatterns {
			if p.id == "fork_bomb" && p.rx.MatchString(command) {
				candidates = append(candidates, command)
			}
		}
	}
	return candidates
}

func literalShellParts(parts []syntax.WordPart) string {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			b.WriteString(literalShellParts(p.Parts))
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func isEmptyGuardObject(v any) bool { m, ok := v.(map[string]any); return ok && len(m) == 0 }

// A WHERE inside a quoted value or comment does not bound a SQL mutation.
func stripSQLLiterals(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\'' || s[i] == '"' || s[i] == '`' {
			quote := s[i]
			out.WriteByte('x')
			i++
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == quote {
					i++
					if i < len(s) && s[i] == quote {
						i++
						continue
					}
					break
				}
				i++
			}
		} else if i+1 < len(s) && s[i:i+2] == "--" {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		} else if i+1 < len(s) && s[i:i+2] == "/*" {
			i += 2
			for i+1 < len(s) && s[i:i+2] != "*/" {
				i++
			}
			if i+1 < len(s) {
				i += 2
			}
			out.WriteByte(' ')
		} else {
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

func truncateGuardText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// Audit metadata never stores restored commands, paths, keys or tool arguments.
// The confirmation returned privately to the calling client includes the command.
func toolFindingEvidence(f toolFinding) string {
	h := sha256.Sum256([]byte(f.command))
	return fmt.Sprintf("%s command_len=%d sha256=%x", f.rule, len(f.command), h[:8])
}

func toolGuardNotice(findings []toolFinding) string {
	if len(findings) == 0 {
		return "工具调用未通过安全检查，请再次确认后重新发起操作。"
	}
	return "检测到危险命令或操作：" + truncateGuardText(findings[0].command, 240) + "。本次工具调用未执行，请再次确认操作范围、备份和权限。确认提示不会自动授权执行；如确需执行，请在客户端审批并调整网关策略后重新发起。"
}
