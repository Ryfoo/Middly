// Command github is a tiny demo client for middly. It fetches a list of
// public repositories from the GitHub API in two passes:
//
//	pass 1: cold — every repo is a cache MISS, real network calls happen.
//	pass 2: warm — every repo is a HIT, served from SQLite in under a ms.
//
// The X-RateLimit-Remaining header is printed verbatim — and middly caches
// it alongside the body, so you'll see the *same* remaining-budget number
// in pass 2 that you saw in pass 1. That's the point: middly replays the
// upstream's bookkeeping without burning your real rate-limit budget.
//
//	# terminal 1
//	./middly --routes='/gh=https://api.github.com'
//
//	# terminal 2 (unauthenticated — limit is 60/hour)
//	go run ./examples/github
//
//	# or authenticated — limit jumps to 5000/hour
//	GITHUB_TOKEN=ghp_... go run ./examples/github
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type repo struct {
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	Stars       int    `json:"stargazers_count"`
	Forks       int    `json:"forks_count"`
	OpenIssues  int    `json:"open_issues_count"`
	Language    string `json:"language"`
	PushedAt    string `json:"pushed_at"`
}

func main() {
	base := flag.String("base", "http://localhost:8080/gh",
		"GitHub API base URL — point at middly, or use https://api.github.com to bypass it")
	reposFlag := flag.String("repos",
		"golang/go,sqlite/sqlite,torvalds/linux,kubernetes/kubernetes,bigskysoftware/htmx",
		"comma-separated owner/repo list")
	passes := flag.Int("passes", 2, "how many times to fetch each repo")
	token := flag.String("token", os.Getenv("GITHUB_TOKEN"),
		"GitHub token (or set GITHUB_TOKEN); raises the rate limit to 5000/hr")
	flag.Parse()

	repos := splitNonEmpty(*reposFlag)
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "no repos given")
		os.Exit(2)
	}

	for p := 1; p <= *passes; p++ {
		fmt.Printf("\npass %d %s\n", p, passLabel(p))
		fmt.Printf("%-30s  %9s  %-12s  %7s  %-5s  %-9s  %s\n",
			"repo", "stars", "language", "issues", "cache", "rate-rem", "latency")
		fmt.Println(strings.Repeat("-", 96))

		for _, name := range repos {
			target := fmt.Sprintf("%s/repos/%s",
				strings.TrimRight(*base, "/"), name)
			start := time.Now()
			r, hdr, status, err := fetch(target, *token)
			dur := time.Since(start)
			if err != nil {
				fmt.Printf("%-30s  ERROR: %v\n", name, err)
				continue
			}
			cache := hdr.Get("X-Cache")
			if cache == "" {
				cache = "MISS"
			}
			rateRem := hdr.Get("X-RateLimit-Remaining")
			if rateRem == "" {
				rateRem = "—"
			}
			if status != http.StatusOK {
				// middly caches errors too — show that.
				fmt.Printf("%-30s  %9s  %-12s  %7s  %-5s  %-9s  %s  (HTTP %d)\n",
					name, "—", "—", "—", cache, rateRem, dur, status)
				continue
			}
			fmt.Printf("%-30s  %9d  %-12s  %7d  %-5s  %-9s  %s\n",
				r.FullName,
				r.Stars,
				fallback(r.Language, "—"),
				r.OpenIssues,
				cache,
				rateRem,
				dur,
			)
		}
	}
	fmt.Println()
	fmt.Println("note: rate-rem is the X-RateLimit-Remaining value GitHub returned")
	fmt.Println("      when the entry was first cached — same value in pass 2 means")
	fmt.Println("      middly served the response without contacting GitHub at all.")
}

func passLabel(p int) string {
	if p == 1 {
		return "(cold — first call hits the network)"
	}
	return "(warm — every call should be a HIT)"
}

func fetch(url, token string) (repo, http.Header, int, error) {
	var r repo
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return r, nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "middly-demo")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return r, nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return r, resp.Header, resp.StatusCode, nil
	}
	err = json.NewDecoder(resp.Body).Decode(&r)
	return r, resp.Header, resp.StatusCode, err
}

func splitNonEmpty(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fallback(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}
