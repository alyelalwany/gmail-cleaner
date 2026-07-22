// Package engine applies a set of Rules to a Gmail account.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/alyelalwany/gmail-cleaner/internal/config"
	"github.com/alyelalwany/gmail-cleaner/internal/gmailclient"
)

type Options struct {
	DryRun      bool
	AllowDelete bool
	// OnlyRule, if non-empty, restricts execution to the rule with this name.
	OnlyRule string
	// PeekN is how many message headers to fetch and print per rule in dry-run.
	PeekN int
}

type RuleResult struct {
	Name       string
	Action     config.Action
	Matched    int
	Acted      int
	Skipped    bool
	SkipReason string
	Duration   time.Duration
	Err        error
}

type Summary struct {
	Results []RuleResult
}

// Run applies every rule in `rules` (or just OnlyRule, if set).
// It returns a Summary even on error so partial progress is visible.
func Run(ctx context.Context, client *gmailclient.Client, rules []config.Rule, opts Options) (Summary, error) {
	var summary Summary
	var firstErr error

	for _, r := range rules {
		if opts.OnlyRule != "" && r.Name != opts.OnlyRule {
			continue
		}
		res := runRule(ctx, client, r, opts)
		summary.Results = append(summary.Results, res)
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
	}
	if opts.OnlyRule != "" && len(summary.Results) == 0 {
		return summary, fmt.Errorf("no rule named %q", opts.OnlyRule)
	}
	return summary, firstErr
}

func runRule(ctx context.Context, client *gmailclient.Client, r config.Rule, opts Options) RuleResult {
	start := time.Now()
	res := RuleResult{Name: r.Name, Action: r.Action}
	log := slog.With("rule", r.Name, "action", string(r.Action))

	if r.Action == config.ActionDelete && !opts.AllowDelete && !opts.DryRun {
		res.Skipped = true
		res.SkipReason = "action=delete requires --allow-delete"
		res.Duration = time.Since(start)
		log.Warn("skipping rule", "reason", res.SkipReason)
		return res
	}

	log.Info("searching", "query", r.Query, "max_per_run", r.MaxPerRun)
	ids, err := client.Search(ctx, r.Query, r.MaxPerRun)
	if err != nil {
		res.Err = err
		res.Duration = time.Since(start)
		log.Error("search failed", "err", err)
		return res
	}
	res.Matched = len(ids)
	log.Info("search complete", "matched", res.Matched)
	if len(ids) == 0 {
		res.Duration = time.Since(start)
		return res
	}

	if opts.DryRun {
		peekN := opts.PeekN
		if peekN <= 0 {
			peekN = 5
		}
		peeks, err := client.PeekMetadata(ctx, ids, peekN)
		if err != nil {
			// Non-fatal: still report the count.
			res.Err = err
			log.Warn("peek failed", "err", err)
		}
		printDryRun(r, ids, peeks)
		res.Duration = time.Since(start)
		return res
	}

	switch r.Action {
	case config.ActionTrash:
		res.Err = client.Trash(ctx, ids)
	case config.ActionArchive:
		res.Err = client.Archive(ctx, ids)
	case config.ActionDelete:
		res.Err = client.Delete(ctx, ids)
	case config.ActionLabel:
		labelID, err := client.EnsureLabel(ctx, r.Label)
		if err != nil {
			res.Err = err
		} else {
			res.Err = client.AddLabel(ctx, ids, labelID)
		}
	default:
		res.Err = errors.New("unknown action (config validation should have caught this)")
	}

	if res.Err == nil {
		res.Acted = len(ids)
		log.Info("rule complete", "acted", res.Acted, "took", time.Since(start).Round(time.Millisecond))
	} else {
		log.Error("rule failed", "err", res.Err, "took", time.Since(start).Round(time.Millisecond))
	}
	res.Duration = time.Since(start)
	return res
}

func printDryRun(r config.Rule, ids []string, peeks []gmailclient.Peek) {
	fmt.Printf("[dry-run] rule=%s action=%s matched=%d (max_per_run=%d)\n",
		r.Name, r.Action, len(ids), r.MaxPerRun)
	fmt.Printf("          query: %s\n", r.Query)
	if r.Action == config.ActionLabel {
		fmt.Printf("          label: %s\n", r.Label)
	}
	for i, p := range peeks {
		fmt.Printf("          [%d] from=%q subject=%q\n", i+1, p.From, p.Subject)
	}
	if len(peeks) < len(ids) {
		fmt.Printf("          ...and %d more\n", len(ids)-len(peeks))
	}
}

// Print writes a human-readable summary of a Run to stdout.
func (s Summary) Print() {
	fmt.Println()
	fmt.Println("Summary:")
	for _, r := range s.Results {
		switch {
		case r.Skipped:
			fmt.Printf("  %-30s SKIPPED (%s)\n", r.Name, r.SkipReason)
		case r.Err != nil:
			fmt.Printf("  %-30s ERROR matched=%d acted=%d took=%s: %v\n",
				r.Name, r.Matched, r.Acted, r.Duration.Round(time.Millisecond), r.Err)
		default:
			fmt.Printf("  %-30s action=%s matched=%d acted=%d took=%s\n",
				r.Name, r.Action, r.Matched, r.Acted, r.Duration.Round(time.Millisecond))
		}
	}
}
