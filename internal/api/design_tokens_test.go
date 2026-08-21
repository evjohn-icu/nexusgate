package api

import (
	"regexp"
	"strings"
	"testing"
)

// hardcodedHexColor matches a hardcoded hexadecimal CSS color literal
// (#rgb / #rgba / #rrggbb / #rrggbbaa). Any such value inside a page's own
// <style> block is a design-token violation: colors must come from the
// shared CSS custom properties (--ui-*, --ev-*, --brand, ...) defined in the
// shell, so the whole app can be re-themed from one place instead of
// per-page literals.
var hardcodedHexColor = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)

// designTokensNotMigrated is the known-not-migrated whitelist for
// TestPageCSSUsesDesignTokens.
//
// ================= 迁移进度白名单 =================
// 每完成一个页面「硬编码颜色 → design token」的迁移任务，就从这里删掉对应
// 的那一项；任务 15（11 个页面全部迁移完成）时，这个 map 必须为空 —— 届时
// 测试自动变成对所有页面全量断言，任何漏网的十六进制颜色都会让测试变红。
// Key 是页面常量名（不是页面 HTML 内容），与 TestPageCSSUsesDesignTokens
// 里遍历的名单一一对应。
// ================================================
var designTokensNotMigrated = map[string]bool{}

// TestPageCSSUsesDesignTokens asserts that each page's own <style> block uses
// design tokens instead of hardcoded hex colors. It reads the page constant
// directly — never through shelledPage — so the shared shellCSS (which is
// intentionally exempt from this rule) is not scanned.
func TestPageCSSUsesDesignTokens(t *testing.T) {
	pages := []struct {
		name string
		html string
	}{
		{"libraryIndexHTML", libraryIndexHTML},
		{"progressHTML", progressHTML},
		{"providersHTML", providersHTML},
		{"settingsHTML", settingsHTML},
		{"workersPageHTML", workersPageHTML},
		{"tagsHTML", tagsHTML},
		{"repurposeWorkspaceHTML", repurposeWorkspaceHTML},
		{"libraryRootsHTML", libraryRootsHTML},
		{"setupHTML", setupHTML},
		{"workerSetupPageHTML", workerSetupPageHTML},
		{"collectionsHTML", collectionsHTML},
	}

	for _, p := range pages {
		css := styleBlockCSS(t, p.html)
		if designTokensNotMigrated[p.name] {
			// Whitelisted: the page is still on the migration backlog. Log the
			// remaining count so the test doubles as a progress bar — the
			// number should shrink towards 0 as each page migrates.
			n := len(hardcodedHexColor.FindAllString(css, -1))
			t.Logf("白名单（未迁移）%s：还剩 %d 个硬编码颜色", p.name, n)
			continue
		}
		matches := hardcodedHexColor.FindAllString(css, -1)
		if len(matches) > 0 {
			t.Errorf("%s 的 <style> 里还有 %d 个硬编码十六进制颜色：%v", p.name, len(matches), matches)
		}
	}
}

// styleBlockCSS extracts the text between the page's <style> and </style>
// tags. Every page constant carries exactly one style block, so the first
// occurrences are the right ones — the same assumption
// TestShellCSSLandsInsideStyleBlock makes.
func styleBlockCSS(t *testing.T, page string) string {
	t.Helper()
	const openTag, closeTag = "<style>", "</style>"
	open := strings.Index(page, openTag)
	close := strings.Index(page, closeTag)
	if open < 0 || close < 0 || close <= open {
		t.Fatalf("page has no <style>...</style> block (open=%d close=%d)", open, close)
	}
	return page[open+len(openTag) : close]
}
