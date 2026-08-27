package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/rsc"
)

type exportDoc struct {
	Provider string          `json:"provider"`
	Accounts []exportAccount `json:"accounts"`
}

type exportAccount struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	SSOToken string `json:"sso_token"`
	Token    string `json:"token"`
}

type row struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	Verdict    string `json:"verdict"`
	Details    string `json:"details,omitempty"`
	Error      string `json:"error,omitempty"`
	ElapsedMs  int64  `json:"elapsed_ms"`
	Suppressed bool   `json:"suppressed,omitempty"`
}

func main() {
	in := flag.String("in", "", "grok2api web export JSON")
	out := flag.String("out", "", "results JSONL path")
	workers := flag.Int("workers", 4, "concurrent probes")
	timeout := flag.Duration("timeout", 35*time.Second, "per-account probe timeout")
	limit := flag.Int("limit", 0, "optional cap on accounts (0 = all)")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: ssoprobebatch -in export.json -out results.jsonl")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	accounts, err := parseExport(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	if *limit > 0 && len(accounts) > *limit {
		accounts = accounts[:*limit]
	}
	outFile, err := os.OpenFile(*out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	defer outFile.Close()

	jobs := make(chan exportAccount)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done atomic.Int64
	total := len(accounts)
	start := time.Now()
	fmt.Fprintf(os.Stderr, "ssoprobebatch accounts=%d workers=%d timeout=%s\n", total, *workers, timeout.String())

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			checker := rsc.NewSSOProbeChecker(*timeout)
			for acc := range jobs {
				token := strings.TrimSpace(acc.SSOToken)
				if token == "" {
					token = strings.TrimSpace(acc.Token)
				}
				r := row{Email: acc.Email, Name: acc.Name}
				t0 := time.Now()
				if token == "" {
					r.Verdict = "error"
					r.Error = "empty sso token"
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
					result := checker.Check(ctx, token)
					cancel()
					r.Verdict = string(result.Verdict)
					r.Details = result.BotFlagDetails
					r.Error = result.Error
					r.Suppressed = result.Suppressed
				}
				r.ElapsedMs = time.Since(t0).Milliseconds()
				line, _ := json.Marshal(r)
				mu.Lock()
				_, _ = outFile.Write(append(line, '\n'))
				mu.Unlock()
				n := done.Add(1)
				if n%10 == 0 || n == int64(total) {
					fmt.Fprintf(os.Stderr, "[%d/%d] %s %s %dms\n", n, total, r.Email, r.Verdict, r.ElapsedMs)
				}
			}
		}()
	}
	for _, acc := range accounts {
		jobs <- acc
	}
	close(jobs)
	wg.Wait()
	fmt.Fprintf(os.Stderr, "done accounts=%d elapsed=%.0fs\n", total, time.Since(start).Seconds())
}

func parseExport(raw []byte) ([]exportAccount, error) {
	var doc exportDoc
	if err := json.Unmarshal(raw, &doc); err == nil && len(doc.Accounts) > 0 {
		return doc.Accounts, nil
	}
	var list []exportAccount
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return list, nil
	}
	return nil, fmt.Errorf("unrecognized export shape")
}
