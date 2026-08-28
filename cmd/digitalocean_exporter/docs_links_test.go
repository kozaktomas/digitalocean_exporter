package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is the repository, reached from this package's directory.
const repoRoot = "../.."

// siteLinkPattern matches a link into the published site. The trailing class
// stops at whatever ends a URL in Markdown, a shell command or a Terraform
// string.
var siteLinkPattern = regexp.MustCompile(`https://kozaktomas\.github\.io/digitalocean_exporter([^)"'` + "`" + `\s>]*)`)

// docsVersionSegment matches the version directory mike publishes each build
// into: an alias such as "latest", or a version such as "0.3".
var docsVersionSegment = regexp.MustCompile(`^(latest|dev|[0-9]+(\.[0-9]+)*)$`)

// The site is versioned: mike publishes every build into its own directory and
// leaves the root as a redirect, so https://…/digitalocean_exporter/install/ is
// a 404 and only https://…/digitalocean_exporter/latest/install/ is a page.
// That is invisible in a Markdown file and the docs build cannot see it either,
// since these links leave the site. This walks every committed Markdown file
// and resolves each link into the site against docs/, so a link that would 404
// fails here instead of in a reader's browser.
func TestSiteLinksResolveToDocumentationPages(t *testing.T) {
	for _, file := range markdownFiles(t) {
		raw, err := os.ReadFile(file) //nolint:gosec // the path comes from a walk of the repository itself.
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		for _, match := range siteLinkPattern.FindAllStringSubmatch(string(raw), -1) {
			link, path := match[0], strings.Trim(match[1], "/")
			// The bare root is the site's entry point and the Helm chart
			// repository, which serves index.yaml from there.
			if path == "" {
				continue
			}

			version, page, _ := strings.Cut(path, "/")
			if !docsVersionSegment.MatchString(version) {
				t.Errorf("%s links to %s, which is missing the version directory: "+
					"the site root only redirects, so a page needs a prefix such as latest/",
					file, link)
				continue
			}

			if source := docsSourceFor(page); source == "" {
				t.Errorf("%s links to %s, which no page in docs/ provides", file, link)
			}
		}
	}
}

// docsSourceFor returns the Markdown source behind a page's URL path, or an
// empty string when nothing provides it. mkdocs serves directory URLs, so
// "configuration/" comes from configuration/index.md.
func docsSourceFor(page string) string {
	candidates := []string{filepath.Join(repoRoot, "docs", "index.md")}
	if page != "" {
		candidates = []string{
			filepath.Join(repoRoot, "docs", page+".md"),
			filepath.Join(repoRoot, "docs", page, "index.md"),
		}
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil { //nolint:gosec // the path is built from the repository's own docs tree.
			return candidate
		}
	}
	return ""
}

// markdownFiles lists the Markdown sources and templates in the repository,
// skipping the build directories the documentation toolchain leaves behind.
func markdownFiles(t *testing.T) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".internal", ".venv", "dist", "dist-chart", "site":
				return fs.SkipDir
			}
			return nil
		}
		if name := entry.Name(); strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".md.gotmpl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Markdown files found")
	}
	return files
}
