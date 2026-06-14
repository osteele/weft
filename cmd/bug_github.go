package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

const (
	githubBugLabel      = "weft-bug"
	githubBugMarker     = "<!-- weft-bug -->"
	githubBugCmdTimeout = 30 * time.Second
)

var runGitHubCLI = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return out, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
		}
		return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	return out, nil
}

type githubBugTracker struct {
	repo string
}

type githubIssue struct {
	Number    int64           `json:"number"`
	Title     string          `json:"title"`
	State     string          `json:"state"`
	Body      string          `json:"body"`
	URL       string          `json:"url"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	ClosedAt  *time.Time      `json:"closedAt"`
	Comments  []githubComment `json:"comments"`
}

type githubComment struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

func newGitHubBugTracker(repo string) githubBugTracker {
	return githubBugTracker{repo: strings.TrimSpace(repo)}
}

func (g githubBugTracker) Report(report db.BugReport) error {
	report.Title = strings.TrimSpace(report.Title)
	if report.Title == "" {
		return fmt.Errorf("bug title is required")
	}

	if report.Fingerprint != "" {
		openIssue, err := g.findIssueByFingerprint(report.Fingerprint, "open")
		if err != nil {
			return err
		}
		if openIssue != nil {
			if err := g.issueComment(openIssue.Number, renderGitHubBugUpdate(report)); err != nil {
				return err
			}
			fmt.Printf("Updated #%d: %s\n", openIssue.Number, openIssue.Title)
			return nil
		}
		closedIssue, err := g.findIssueByFingerprint(report.Fingerprint, "closed")
		if err != nil {
			return err
		}
		if closedIssue != nil {
			return fmt.Errorf("bug fingerprint %q belongs to closed GitHub issue #%d; run `weft bug reopen #%d` or choose a different --fingerprint", report.Fingerprint, closedIssue.Number, closedIssue.Number)
		}
	}

	body := renderGitHubBugBody(report)
	args := []string{"issue", "create", "--title", report.Title, "--body", body}
	if g.ensureBugLabel() == nil {
		args = append(args, "--label", githubBugLabel)
	}
	out, err := g.gh(args...)
	if err != nil {
		return err
	}
	issueNumber := parseGitHubIssueNumber(string(out))
	if issueNumber == 0 {
		fmt.Print(strings.TrimSpace(string(out)))
		if len(out) > 0 {
			fmt.Println()
		}
		return nil
	}
	fmt.Printf("Reported #%d: %s\n", issueNumber, report.Title)
	return nil
}

func (g githubBugTracker) Note(id, note string) error {
	number, err := parseGitHubBugID(id)
	if err != nil {
		return err
	}
	if err := g.issueComment(number, note); err != nil {
		return err
	}
	fmt.Printf("Added note to #%d\n", number)
	return nil
}

func (g githubBugTracker) List(all bool) error {
	issues, err := g.listIssues(all, "--label", githubBugLabel)
	if err != nil {
		issues, err = g.listIssues(all, "--search", "weft-bug")
	}
	if err != nil {
		return err
	}
	if len(issues) == 0 {
		fmt.Println("No bugs.")
		return nil
	}
	for _, issue := range issues {
		updated := issue.UpdatedAt.Unix()
		fmt.Printf("#%-5d %-7s %-10s %s\n", issue.Number, strings.ToLower(issue.State), ageString(updated), issue.Title)
	}
	return nil
}

func (g githubBugTracker) Show(id string) error {
	number, err := parseGitHubBugID(id)
	if err != nil {
		return err
	}
	args := []string{"issue", "view", strconv.FormatInt(number, 10), "--json", "number,title,state,body,createdAt,updatedAt,closedAt,url,comments"}
	out, err := g.gh(args...)
	if err != nil {
		return err
	}
	var issue githubIssue
	if err := json.Unmarshal(out, &issue); err != nil {
		return fmt.Errorf("parse GitHub issue: %w", err)
	}
	printGitHubIssue(issue)
	return nil
}

func (g githubBugTracker) Close(id, reason string) error {
	number, err := parseGitHubBugID(id)
	if err != nil {
		return err
	}
	args := []string{"issue", "close", strconv.FormatInt(number, 10)}
	if reason = strings.TrimSpace(reason); reason != "" {
		args = append(args, "--comment", "Closed by `weft bug close`.\n\nReason: "+reason)
	}
	if _, err := g.gh(args...); err != nil {
		return err
	}
	fmt.Printf("Closed #%d\n", number)
	return nil
}

func (g githubBugTracker) Reopen(id string) error {
	number, err := parseGitHubBugID(id)
	if err != nil {
		return err
	}
	if _, err := g.gh("issue", "reopen", strconv.FormatInt(number, 10)); err != nil {
		return err
	}
	fmt.Printf("Reopened #%d\n", number)
	return nil
}

func (g githubBugTracker) findIssueByFingerprint(fingerprint, state string) (*githubIssue, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil, nil
	}
	issues, err := g.listIssuesBySearch(state, githubFingerprintSearch(fingerprint))
	if err != nil {
		return nil, err
	}
	marker := githubFingerprintMarker(fingerprint)
	for _, issue := range issues {
		if strings.Contains(issue.Body, marker) || strings.Contains(issue.Body, fingerprint) {
			return &issue, nil
		}
	}
	return nil, nil
}

func (g githubBugTracker) listIssues(all bool, filter ...string) ([]githubIssue, error) {
	state := "open"
	if all {
		state = "all"
	}
	args := []string{"issue", "list", "--state", state, "--limit", "100", "--json", "number,title,state,updatedAt,url"}
	args = append(args, filter...)
	out, err := g.gh(args...)
	if err != nil {
		return nil, err
	}
	var issues []githubIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse GitHub issue list: %w", err)
	}
	return issues, nil
}

func (g githubBugTracker) listIssuesBySearch(state, query string) ([]githubIssue, error) {
	args := []string{"issue", "list", "--state", state, "--limit", "20", "--search", query, "--json", "number,title,state,body,updatedAt,url"}
	out, err := g.gh(args...)
	if err != nil {
		return nil, err
	}
	var issues []githubIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse GitHub issue search: %w", err)
	}
	return issues, nil
}

func (g githubBugTracker) issueComment(number int64, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("bug note is required")
	}
	_, err := g.gh("issue", "comment", strconv.FormatInt(number, 10), "--body", body)
	return err
}

func (g githubBugTracker) ensureBugLabel() error {
	_, err := g.gh("label", "create", githubBugLabel, "--description", "Weft bug tracker", "--color", "C2E0C6", "--force")
	return err
}

func (g githubBugTracker) gh(args ...string) ([]byte, error) {
	if g.repo != "" {
		args = append(args, "--repo", g.repo)
	}
	ctx, cancel := context.WithTimeout(context.Background(), githubBugCmdTimeout)
	defer cancel()
	return runGitHubCLI(ctx, args...)
}

func parseGitHubBugID(s string) (int64, error) {
	raw := strings.TrimSpace(strings.ToLower(s))
	raw = strings.TrimPrefix(raw, "#")
	raw = strings.TrimPrefix(raw, "gh")
	raw = strings.TrimPrefix(raw, "wb")
	if raw == "" {
		return 0, fmt.Errorf("empty GitHub issue id")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid GitHub issue id %q", s)
	}
	return id, nil
}

func parseGitHubIssueNumber(out string) int64 {
	re := regexp.MustCompile(`/issues/([0-9]+)`)
	match := re.FindStringSubmatch(out)
	if len(match) != 2 {
		return 0
	}
	id, _ := strconv.ParseInt(match[1], 10, 64)
	return id
}

func renderGitHubBugBody(report db.BugReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "## Weft Bug")
	fmt.Fprintln(&b)
	writeGitHubBugField(&b, "Kind", report.Kind)
	writeGitHubBugField(&b, "Scope", report.Scope)
	writeGitHubBugField(&b, "Likelihood", report.Likelihood)
	writeGitHubBugField(&b, "Severity", report.Severity)
	writeGitHubBugField(&b, "Fingerprint", report.Fingerprint)
	if report.JobID != nil {
		writeGitHubBugField(&b, "Job", ids.FormatJobID(*report.JobID))
	}
	writeGitHubBugField(&b, "Host", report.Host)
	writeGitHubBugField(&b, "Summary", report.Summary)
	if strings.TrimSpace(report.Detail) != "" {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "### Detail")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, strings.TrimSpace(report.Detail))
	}
	if strings.TrimSpace(report.Note) != "" {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "### Initial Note")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, strings.TrimSpace(report.Note))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, githubBugMarker)
	if strings.TrimSpace(report.Fingerprint) != "" {
		fmt.Fprintln(&b, githubFingerprintMarker(report.Fingerprint))
	}
	return strings.TrimSpace(b.String())
}

func renderGitHubBugUpdate(report db.BugReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "Observed again via `weft bug report`.")
	fmt.Fprintln(&b)
	writeGitHubBugField(&b, "Kind", report.Kind)
	writeGitHubBugField(&b, "Scope", report.Scope)
	writeGitHubBugField(&b, "Likelihood", report.Likelihood)
	writeGitHubBugField(&b, "Severity", report.Severity)
	if report.JobID != nil {
		writeGitHubBugField(&b, "Job", ids.FormatJobID(*report.JobID))
	}
	writeGitHubBugField(&b, "Host", report.Host)
	writeGitHubBugField(&b, "Summary", report.Summary)
	writeGitHubBugField(&b, "Fingerprint", report.Fingerprint)
	if strings.TrimSpace(report.Detail) != "" {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "### Detail")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, strings.TrimSpace(report.Detail))
	}
	if strings.TrimSpace(report.Note) != "" {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "### Note")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, strings.TrimSpace(report.Note))
	}
	return strings.TrimSpace(b.String())
}

func writeGitHubBugField(b *strings.Builder, name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	fmt.Fprintf(b, "- %s: %s\n", name, value)
}

func githubFingerprintSearch(fingerprint string) string {
	return "weft-bug-fingerprint:" + strings.TrimSpace(fingerprint)
}

func githubFingerprintMarker(fingerprint string) string {
	return "<!-- " + githubFingerprintSearch(fingerprint) + " -->"
}

func printGitHubIssue(issue githubIssue) {
	fmt.Printf("Bug ID:      #%d\n", issue.Number)
	fmt.Printf("Status:      %s\n", strings.ToLower(issue.State))
	fmt.Printf("Title:       %s\n", issue.Title)
	if issue.URL != "" {
		fmt.Printf("URL:         %s\n", issue.URL)
	}
	if !issue.CreatedAt.IsZero() {
		fmt.Printf("Created:     %s\n", issue.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if !issue.UpdatedAt.IsZero() {
		fmt.Printf("Updated:     %s\n", issue.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if issue.ClosedAt != nil && !issue.ClosedAt.IsZero() {
		fmt.Printf("Closed:      %s\n", issue.ClosedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if strings.TrimSpace(issue.Body) != "" {
		fmt.Println("Detail:")
		fmt.Println(strings.TrimSpace(issue.Body))
	}
	if len(issue.Comments) > 0 {
		fmt.Println()
		fmt.Println("Notes:")
		for _, comment := range issue.Comments {
			author := strings.TrimSpace(comment.Author.Login)
			if author == "" {
				author = "unknown"
			}
			fmt.Printf("- %s %s: %s\n", comment.CreatedAt.Local().Format("2006-01-02 15:04:05"), author, firstGitHubBugCommentLine(comment.Body))
		}
	}
}

func firstGitHubBugCommentLine(s string) string {
	s = strings.TrimSpace(s)
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimSpace(line)
	}
	return s
}
