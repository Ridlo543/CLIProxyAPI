package contextcompression

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	detectWindow    = 1024
	dedupLineMax    = 2000
	statusMaxFiles  = 10
	grepPerFileMax  = 10
	findPerDirMax   = 10
	findTotalDirMax = 20
	treeMaxLines    = 200
	smartHead       = 120
	smartTail       = 60
	smartMinLines   = 250
)

type namedFilter struct {
	name  string
	apply func(string) string
}

var (
	gitLogMarker    = regexp.MustCompile(`(?m)^[*|/\\ ]*commit [0-9a-f]{7,40}$`)
	gitDiffMarker   = regexp.MustCompile(`(?m)^diff --git |^@@ `)
	gitStatusMarker = regexp.MustCompile(`(?m)^On branch |^nothing to commit|^Changes (not |to be )|^Untracked files:`)
	porcelainLine   = regexp.MustCompile(`^[ MADRCU?!][ MADRCU?!] \S`)
	buildMarker     = regexp.MustCompile(`(?im)^(npm (warn|error|ERR!)|yarn (warn|error)|\s*Compiling\s+\S+|\s*Downloading\s+\S+|added \d+ package|\[ERROR\]|BUILD (SUCCESS|FAILED)|\s*Finished\s+|Successfully (installed|built)|ERROR:)`)
	grepLineRE      = regexp.MustCompile(`^([^:]+):(\d+):(.*)$`)
	searchHeaderRE  = regexp.MustCompile(`^Result of search in '[^']*' \(total (\d+) files?\):`)
	readNumberedRE  = regexp.MustCompile(`^\s*\d+\|`)
	lsDateRE        = regexp.MustCompile(`\s+(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\s+(\d{4}|\d{2}:\d{2})\s+`)
)

func applyRTK(slots []slot, min int, stats *Stats) bool {
	type staged struct {
		s   slot
		out string
	}
	pending := make([]staged, 0, len(slots))
	for _, s := range slots {
		before := len([]byte(s.text))
		stats.BytesBefore += before
		if before < min {
			stats.BytesAfter += before
			continue
		}
		filter := autoDetectFilter(s.text)
		if filter == nil {
			stats.BytesAfter += before
			continue
		}
		out := safeApply(filter, s.text)
		after := len([]byte(out))
		if out == "" || after >= before {
			stats.BytesAfter += before
			continue
		}
		stats.BytesAfter += after
		pending = append(pending, staged{s, out})
	}
	if len(pending) == 0 {
		stats.Reason = "no_eligible"
		return false
	}
	for _, p := range pending {
		p.s.write(p.out)
	}
	stats.Compressed = len(pending)
	return true
}

func safeApply(filter *namedFilter, input string) (out string) {
	out = input
	defer func() {
		if recover() != nil {
			out = input
		}
	}()
	candidate := filter.apply(input)
	if candidate != "" {
		out = candidate
	}
	return out
}

func autoDetectFilter(text string) *namedFilter {
	head := text
	runes := []rune(head)
	if len(runes) > detectWindow {
		head = string(runes[:detectWindow])
	}
	if gitLogMarker.MatchString(head) {
		return &namedFilter{"git-log", gitLogFilter}
	}
	if gitDiffMarker.MatchString(head) {
		return &namedFilter{"git-diff", gitDiff}
	}
	if gitStatusMarker.MatchString(head) {
		return &namedFilter{"git-status", gitStatus}
	}
	if buildMarker.MatchString(head) {
		return &namedFilter{"build-output", buildOutput}
	}
	lines := strings.Split(head, "\n")
	nonEmpty := make([]string, 0, len(lines))
	porcelain := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmpty = append(nonEmpty, line)
			if porcelainLine.MatchString(line) {
				porcelain++
			}
		}
	}
	if len(nonEmpty) >= 3 && float64(porcelain)/float64(len(nonEmpty)) >= .6 {
		return &namedFilter{"git-status", gitStatus}
	}
	for i, line := range nonEmpty {
		if i >= 5 {
			break
		}
		if grepLineRE.MatchString(line) {
			return &namedFilter{"grep", grepFilter}
		}
	}
	if len(nonEmpty) >= 3 {
		all := true
		for _, line := range nonEmpty {
			v := strings.TrimSpace(line)
			windowsPath := regexp.MustCompile(`^[A-Za-z]:[\\/]`).MatchString(v)
			if (!windowsPath && strings.Contains(v, ":")) || !(windowsPath || strings.HasPrefix(v, ".") || strings.HasPrefix(v, "/") || strings.Contains(v, "/")) {
				all = false
				break
			}
		}
		if all {
			return &namedFilter{"find", findFilter}
		}
	}
	if strings.Contains(head, "├──") || strings.Contains(head, "└──") || strings.Contains(head, "│  ") {
		return &namedFilter{"tree", treeFilter}
	}
	lsRows := 0
	for _, line := range lines {
		if regexp.MustCompile(`^[-dlbcps][rwx-]{9}`).MatchString(line) {
			lsRows++
		}
	}
	if regexp.MustCompile(`(?m)^total \d+$`).MatchString(head) || lsRows >= 3 {
		return &namedFilter{"ls", lsFilter}
	}
	if searchHeaderRE.MatchString(head) {
		return &namedFilter{"search-list", searchListFilter}
	}
	if len(lines) >= smartMinLines && mostlyNumbered(lines) {
		return &namedFilter{"read-numbered", readNumberedFilter}
	}
	if len(nonEmpty) >= 5 {
		return &namedFilter{"dedup-log", dedupFilter}
	}
	if len(strings.Split(text, "\n")) >= smartMinLines {
		return &namedFilter{"smart-truncate", smartTruncateFilter}
	}
	return nil
}

func gitLogFilter(input string) string {
	if input == "" {
		return ""
	}
	lines := strings.Split(input, "\n")
	out := []string{}
	skipped := 0
	inCommit, subjectSeen := false, false
	push := func(line string) {
		if len(out) < 200 {
			out = append(out, line)
		} else {
			skipped++
		}
	}
	commitRE := regexp.MustCompile(`(?i)^(commit [0-9a-f]{7,40}|[*|/\\ ]+commit [0-9a-f]{7,40})$`)
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r\t ")
		trimmed := strings.TrimSpace(line)
		if commitRE.MatchString(trimmed) {
			inCommit = true
			subjectSeen = false
			push(line)
			continue
		}
		if inCommit {
			if regexp.MustCompile(`(?i)^[*|/\\ ]*(Author|Date):`).MatchString(trimmed) {
				push(trimmed)
				continue
			}
			if trimmed == "" {
				continue
			}
			if !subjectSeen && isGitLogSubject(line) {
				push("  Subject: " + trimmed)
				subjectSeen = true
				continue
			}
			if regexp.MustCompile(`^\d+ file\w* changed`).MatchString(trimmed) {
				push("  " + trimmed)
				continue
			}
			if strings.HasPrefix(trimmed, "diff --git ") {
				push("  ... diff body omitted")
			}
			continue
		}
		if m := regexp.MustCompile(`(?i)^[*|/\\ ]+([0-9a-f]{7,40}\s+.+)`).FindStringSubmatch(trimmed); m != nil {
			push(m[1])
			continue
		}
		if regexp.MustCompile(`(?i)^[0-9a-f]{7,40}\s+`).MatchString(trimmed) {
			push(trimmed)
			continue
		}
		if regexp.MustCompile(`^[*|/\\ ]+$`).MatchString(trimmed) && strings.ContainsAny(trimmed, "*|/\\") {
			continue
		}
		push(trimmed)
	}
	if skipped > 0 {
		out = append(out, fmt.Sprintf("... (%d more lines)", skipped))
	}
	result := strings.Join(out, "\n")
	if result == "" {
		return input
	}
	if len(result) > len(input) {
		return input
	}
	return result
}

func isGitLogSubject(line string) bool {
	for i := 0; i+4 < len(line); i++ {
		valid := true
		for _, r := range line[:i] {
			if !strings.ContainsRune("*|/\\ ", r) {
				valid = false
				break
			}
		}
		if valid && line[i:i+4] == "    " && !strings.ContainsRune(" \t\r\n", rune(line[i+4])) {
			return true
		}
	}
	return false
}

func mostlyNumbered(lines []string) bool {
	sample := lines
	if len(sample) > 100 {
		sample = sample[:100]
	}
	hits, n := 0, 0
	for _, line := range sample {
		if line == "" {
			continue
		}
		n++
		if readNumberedRE.MatchString(line) {
			hits++
		}
	}
	return n >= 5 && float64(hits)/float64(n) >= .7
}

func grepFilter(input string) string {
	by := map[string][][2]string{}
	total := 0
	for _, line := range strings.Split(input, "\n") {
		m := grepLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		total++
		by[m[1]] = append(by[m[1]], [2]string{m[2], m[3]})
	}
	if total == 0 {
		return input
	}
	files := make([]string, 0, len(by))
	for f := range by {
		files = append(files, f)
	}
	sort.Strings(files)
	var b strings.Builder
	fmt.Fprintf(&b, "%d matches in %dF:\n\n", total, len(files))
	for _, f := range files {
		matches := by[f]
		fmt.Fprintf(&b, "[file] %s (%d):\n", f, len(matches))
		limit := len(matches)
		if limit > grepPerFileMax {
			limit = grepPerFileMax
		}
		for _, m := range matches[:limit] {
			fmt.Fprintf(&b, "  %4s: %s\n", m[0], strings.TrimSpace(m[1]))
		}
		if len(matches) > limit {
			fmt.Fprintf(&b, "  +%d\n", len(matches)-limit)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func findFilter(input string) string {
	return groupedPaths(input, " files in ", findPerDirMax, findTotalDirMax)
}
func groupedPaths(input, label string, perDir, maxDirs int) string {
	lines := nonBlank(strings.Split(input, "\n"))
	if len(lines) == 0 {
		return input
	}
	by := map[string][]string{}
	for _, p := range lines {
		i := strings.LastIndex(p, "/")
		if backslash := strings.LastIndex(p, "\\"); backslash > i {
			i = backslash
		}
		dir, name := ".", p
		if i >= 0 {
			dir = p[:i]
			if dir == "" {
				dir = "/"
			}
			name = p[i+1:]
		}
		by[dir] = append(by[dir], name)
	}
	dirs := sortedKeys(by)
	var b strings.Builder
	fmt.Fprintf(&b, "%d%s%d dirs:\n\n", len(lines), label, len(dirs))
	show := dirs
	if len(show) > maxDirs {
		show = show[:maxDirs]
	}
	for _, d := range show {
		names := by[d]
		fmt.Fprintf(&b, "%s/ (%d):\n", strings.ReplaceAll(d, "\\", "/"), len(names))
		n := len(names)
		if n > perDir {
			n = perDir
		}
		for _, name := range names[:n] {
			fmt.Fprintf(&b, "  %s\n", name)
		}
		if len(names) > n {
			fmt.Fprintf(&b, "  +%d\n", len(names)-n)
		}
		b.WriteByte('\n')
	}
	if len(dirs) > len(show) {
		fmt.Fprintf(&b, "+%d more dirs\n", len(dirs)-len(show))
	}
	return b.String()
}

func treeFilter(input string) string {
	out := []string{}
	for _, line := range strings.Split(input, "\n") {
		if strings.Contains(line, "director") && strings.Contains(line, "file") {
			continue
		}
		if strings.TrimSpace(line) == "" && len(out) == 0 {
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > treeMaxLines {
		return strings.Join(out[:treeMaxLines], "\n") + fmt.Sprintf("\n... +%d more lines", len(out)-treeMaxLines)
	}
	return strings.Join(out, "\n")
}

func searchListFilter(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) == 0 {
		return input
	}
	paths := []string{}
	for _, line := range lines[1:] {
		v := strings.TrimSpace(line)
		if strings.HasPrefix(v, "- ") {
			paths = append(paths, v[2:])
		}
	}
	if len(paths) == 0 {
		return input
	}
	by := map[string][]string{}
	for _, p := range paths {
		i := strings.LastIndex(p, "/")
		if backslash := strings.LastIndex(p, "\\"); backslash > i {
			i = backslash
		}
		dir, name := ".", p
		if i >= 0 {
			dir, name = p[:i], p[i+1:]
			if dir == "" {
				dir = "/"
			}
		}
		by[dir] = append(by[dir], name)
	}
	dirs := sortedKeys(by)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%d files in %d dirs:\n\n", lines[0], len(paths), len(dirs))
	show := dirs
	if len(show) > 20 {
		show = show[:20]
	}
	for _, dir := range show {
		names := by[dir]
		fmt.Fprintf(&b, "%s/ (%d):\n", dir, len(names))
		n := len(names)
		if n > 10 {
			n = 10
		}
		for _, name := range names[:n] {
			fmt.Fprintf(&b, "  %s\n", name)
		}
		if len(names) > n {
			fmt.Fprintf(&b, "  +%d\n", len(names)-n)
		}
		b.WriteByte('\n')
	}
	if len(dirs) > len(show) {
		fmt.Fprintf(&b, "+%d more dirs\n", len(dirs)-len(show))
	}
	return strings.TrimRight(b.String(), "\n")
}

func readNumberedFilter(input string) string {
	return smartTruncate(input, "... +%d lines truncated (file continues)")
}
func smartTruncateFilter(input string) string { return smartTruncate(input, "... +%d lines truncated") }
func smartTruncate(input, marker string) string {
	lines := strings.Split(input, "\n")
	if len(lines) < smartMinLines {
		return input
	}
	return strings.Join(append(append(append([]string{}, lines[:smartHead]...), fmt.Sprintf(marker, len(lines)-smartHead-smartTail)), lines[len(lines)-smartTail:]...), "\n")
}

func dedupFilter(input string) string {
	lines := strings.Split(input, "\n")
	out := []string{}
	prev := ""
	hasPrev := false
	run, blank := 0, 0
	flush := func() {
		if hasPrev && run > 1 {
			out = append(out, fmt.Sprintf("  ... (%d duplicate lines)", run-1))
		}
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if blank < 1 {
				out = append(out, line)
			}
			blank++
			flush()
			hasPrev = false
			run = 0
			continue
		}
		blank = 0
		if hasPrev && line == prev {
			run++
			continue
		}
		flush()
		out = append(out, line)
		prev = line
		hasPrev = true
		run = 1
		if len(out) >= dedupLineMax {
			return strings.Join(append(out, fmt.Sprintf("... (truncated at %d lines)", dedupLineMax)), "\n")
		}
	}
	flush()
	return strings.Join(out, "\n")
}

func gitDiff(input string) string {
	result := []string{}
	file := ""
	added, removed, shown, skipped := 0, 0, 0, 0
	inHunk, truncated := false, false
	flush := func() {
		if skipped > 0 {
			result = append(result, fmt.Sprintf("  ... (%d lines truncated)", skipped))
			truncated = true
			skipped = 0
		}
		if file != "" && (added > 0 || removed > 0) {
			result = append(result, fmt.Sprintf("  +%d -%d", added, removed))
		}
	}
	for _, line := range strings.Split(input, "\n") {
		if strings.HasPrefix(line, "diff --git") {
			flush()
			parts := strings.Split(line, " b/")
			file = "unknown"
			if len(parts) > 1 {
				file = strings.Join(parts[1:], " b/")
			}
			result = append(result, "\n"+file)
			added, removed, shown = 0, 0, 0
			inHunk = false
		} else if strings.HasPrefix(line, "@@") {
			if skipped > 0 {
				result = append(result, fmt.Sprintf("  ... (%d lines truncated)", skipped))
				truncated = true
				skipped = 0
			}
			inHunk = true
			shown = 0
			result = append(result, "  "+line)
		} else if inHunk {
			changed := strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---")
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added++
			}
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				removed++
			}
			if changed {
				if shown < 100 {
					result = append(result, "  "+line)
					shown++
				} else {
					skipped++
				}
			} else if shown > 0 && shown < 100 && !strings.HasPrefix(line, "\\") {
				result = append(result, "  "+line)
				shown++
			}
		}
		if len(result) >= 500 {
			result = append(result, "\n... (more changes truncated)")
			truncated = true
			break
		}
	}
	flush()
	if truncated {
		result = append(result, "[full diff: rtk git diff --no-compact]")
	}
	return strings.Join(result, "\n")
}

func gitStatus(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		return "Clean working tree"
	}
	branch := ""
	staged, modified, untracked := []string{}, []string{}, []string{}
	conflicts := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "On branch ") {
			branch = strings.TrimPrefix(line, "On branch ")
			continue
		}
		if strings.HasPrefix(line, "##") {
			branch = strings.TrimSpace(strings.TrimPrefix(line, "##"))
			continue
		}
		if len(line) >= 3 && porcelainLine.MatchString(line) {
			file := line[3:]
			if line[:2] == "??" {
				untracked = append(untracked, file)
				continue
			}
			if strings.ContainsRune("MADRC", rune(line[0])) {
				staged = append(staged, file)
			} else if line[0] == 'U' {
				conflicts++
			}
			if line[1] == 'M' || line[1] == 'D' {
				modified = append(modified, file)
			}
			continue
		}
		if match := regexp.MustCompile(`^\s*(modified|new file|deleted|renamed|both modified):\s+(.+)$`).FindStringSubmatch(line); match != nil {
			switch match[1] {
			case "both modified":
				conflicts++
			case "modified", "deleted":
				modified = append(modified, strings.TrimSpace(match[2]))
			case "new file", "renamed":
				staged = append(staged, strings.TrimSpace(match[2]))
			}
		}
	}
	var b strings.Builder
	if branch != "" {
		fmt.Fprintf(&b, "* %s\n", branch)
	}
	writeStatus(&b, "+ Staged", staged)
	writeStatus(&b, "~ Modified", modified)
	writeStatus(&b, "? Untracked", untracked)
	if conflicts > 0 {
		fmt.Fprintf(&b, "conflicts: %d files\n", conflicts)
	}
	if len(staged)+len(modified)+len(untracked)+conflicts == 0 {
		b.WriteString("clean — nothing to commit\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
func writeStatus(b *strings.Builder, label string, files []string) {
	if len(files) == 0 {
		return
	}
	fmt.Fprintf(b, "%s: %d files\n", label, len(files))
	n := len(files)
	if n > statusMaxFiles {
		n = statusMaxFiles
	}
	for _, f := range files[:n] {
		fmt.Fprintf(b, "   %s\n", f)
	}
	if len(files) > n {
		fmt.Fprintf(b, "   ... +%d more\n", len(files)-n)
	}
}

func buildOutput(input string) string {
	lines := strings.Split(input, "\n")
	errors, warnings, deps := []string{}, []string{}, []string{}
	summary := []string{}
	compiling, downloading := 0, 0
	inCargoError := false
	for _, line := range lines {
		v := strings.TrimSpace(line)
		low := strings.ToLower(v)
		if inCargoError {
			if v == "" {
				inCargoError = false
				continue
			}
			if regexp.MustCompile(`^\s*(-->|\||\d+\s*\||=)`).MatchString(line) {
				errors = append(errors, line)
				continue
			}
			inCargoError = false
		}
		switch {
		case strings.HasPrefix(low, "npm warn deprecated"):
			deps = append(deps, line)
		case strings.HasPrefix(low, "npm err") || strings.HasPrefix(low, "npm error") || strings.HasPrefix(low, "yarn error") || strings.HasPrefix(low, "error:") || strings.HasPrefix(low, "error[") || strings.HasPrefix(low, "[error]") || strings.HasPrefix(low, "build failed"):
			errors = append(errors, line)
			if strings.HasPrefix(low, "error:") || strings.HasPrefix(low, "error[") {
				inCargoError = true
			}
		case strings.HasPrefix(low, "npm warn") || strings.HasPrefix(low, "yarn warn") || strings.HasPrefix(low, "warning:") || strings.HasPrefix(low, "warning[") || strings.HasPrefix(low, "[warning]"):
			warnings = append(warnings, line)
			if strings.HasPrefix(low, "warning:") || strings.HasPrefix(low, "warning[") {
				inCargoError = true
			}
		case strings.HasPrefix(low, "compiling "):
			compiling++
		case strings.HasPrefix(low, "downloading ") || strings.HasPrefix(low, "fetching "):
			downloading++
		case regexp.MustCompile(`(?i)^(added|removed|changed|audited|installed) \d+ package|^finished |^build success|^successfully (installed|built)`).MatchString(v):
			summary = append(summary, line)
		}
	}
	out := []string{}
	n := len(deps)
	if n > 3 {
		n = 3
	}
	out = append(out, deps[:n]...)
	if len(deps) > n {
		out = append(out, fmt.Sprintf("... +%d more deprecated packages", len(deps)-n))
	}
	if compiling > 0 {
		out = append(out, fmt.Sprintf("Compiled %d packages", compiling))
	}
	if downloading > 0 {
		out = append(out, fmt.Sprintf("Downloaded %d packages", downloading))
	}
	out = append(out, errors...)
	n = len(warnings)
	if n > 5 {
		n = 5
	}
	out = append(out, warnings[:n]...)
	if len(warnings) > n {
		out = append(out, fmt.Sprintf("... +%d more warnings", len(warnings)-n))
	}
	out = append(out, summary...)
	if len(out) == 0 {
		return input
	}
	return strings.Join(out, "\n")
}

func lsFilter(input string) string {
	dirs := []string{}
	files := [][2]string{}
	ext := map[string]int{}
	noise := map[string]bool{"node_modules": true, ".git": true, "target": true, "__pycache__": true, ".next": true, "dist": true, "build": true, ".venv": true, "venv": true, ".cache": true, ".idea": true, ".vscode": true, ".DS_Store": true}
	for _, line := range strings.Split(input, "\n") {
		loc := lsDateRE.FindStringIndex(line)
		if loc == nil {
			continue
		}
		name := line[loc[1]:]
		if name == "." || name == ".." || noise[name] {
			continue
		}
		fields := strings.Fields(line[:loc[0]])
		if len(fields) < 4 {
			continue
		}
		kind := fields[0][0]
		size := 0
		for i := len(fields) - 1; i >= 0; i-- {
			if n, e := strconv.Atoi(fields[i]); e == nil {
				size = n
				break
			}
		}
		if kind == 'd' {
			dirs = append(dirs, name)
		} else if kind == '-' || kind == 'l' {
			files = append(files, [2]string{name, humanSize(size)})
			suffix := "no ext"
			if i := strings.LastIndex(name, "."); i > 0 {
				suffix = name[i:]
			}
			ext[suffix]++
		}
	}
	if len(dirs)+len(files) == 0 {
		return input
	}
	var b strings.Builder
	for _, d := range dirs {
		fmt.Fprintf(&b, "%s/\n", d)
	}
	for _, f := range files {
		fmt.Fprintf(&b, "%s  %s\n", f[0], f[1])
	}
	fmt.Fprintf(&b, "\nSummary: %d files, %d dirs", len(files), len(dirs))
	if len(ext) > 0 {
		type pair struct {
			name  string
			count int
		}
		pairs := make([]pair, 0, len(ext))
		for name, count := range ext {
			pairs = append(pairs, pair{name, count})
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].count > pairs[j].count })
		n := len(pairs)
		if n > 5 {
			n = 5
		}
		parts := make([]string, 0, n)
		for _, p := range pairs[:n] {
			parts = append(parts, fmt.Sprintf("%d %s", p.count, p.name))
		}
		b.WriteString(" (" + strings.Join(parts, ", "))
		if len(pairs) > n {
			fmt.Fprintf(&b, ", +%d more", len(pairs)-n)
		}
		b.WriteString(")")
	}
	return b.String()
}
func humanSize(n int) string {
	if n >= 1048576 {
		return fmt.Sprintf("%.1fM", float64(n)/1048576)
	}
	if n >= 1024 {
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}
func nonBlank(in []string) []string {
	out := []string{}
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
