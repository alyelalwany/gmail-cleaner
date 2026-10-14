package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alyelalwany/gmail-cleaner/internal/auth"
	"github.com/alyelalwany/gmail-cleaner/internal/config"
	"github.com/alyelalwany/gmail-cleaner/internal/engine"
	"github.com/alyelalwany/gmail-cleaner/internal/gmailclient"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "gmail-cleaner",
		Short: "Clean up your Gmail inbox by rule.",
		Long: "gmail-cleaner runs a set of YAML-defined rules against your Gmail account " +
			"to archive, trash, delete, or label messages matching Gmail search queries.",
		SilenceUsage: true,
	}

	var configDir string
	root.PersistentFlags().StringVar(&configDir, "config-dir", "",
		"directory holding credentials.json + token.json (default: ~/.config/gmail-cleaner)")

	resolveDir := func() (string, error) {
		if configDir != "" {
			return configDir, nil
		}
		return auth.DefaultConfigDir()
	}

	root.AddCommand(authCmd(resolveDir))
	root.AddCommand(rulesCmd())
	root.AddCommand(runCmd(resolveDir))
	root.AddCommand(statsCmd(resolveDir))
	root.AddCommand(countCmd(resolveDir))
	root.AddCommand(topSendersCmd(resolveDir))
	root.AddCommand(labelCmd(resolveDir))
	root.AddCommand(purgeCmd(resolveDir))
	root.AddCommand(untrashCmd(resolveDir))
	return root
}

func authCmd(resolveDir func() (string, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "auth",
		Short: "Run the interactive OAuth flow and cache a refresh token.",
		Long: "Opens a browser window to let you grant access to your Gmail account. " +
			"The refresh token is cached at ~/.config/gmail-cleaner/token.json and " +
			"reused by every subsequent command. Re-run this if scopes change or the " +
			"token expires.",
		Example: "  # First-time setup after dropping credentials.json into ~/.config/gmail-cleaner/\n" +
			"  gmail-cleaner auth\n" +
			"\n" +
			"  # Re-auth (e.g. after adding a new scope, or after the 7-day Testing-mode expiry)\n" +
			"  gmail-cleaner auth",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := resolveDir()
			if err != nil {
				return err
			}
			return auth.Login(cmd.Context(), dir)
		},
	}
}

func rulesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rules",
		Short: "Inspect the rules file.",
	}
	var path string
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate the rules file without touching Gmail.",
		Example: "  gmail-cleaner rules validate -c rules.yaml\n" +
			"  gmail-cleaner rules validate -c rules.example.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.Load(path)
			if err != nil {
				return err
			}
			fmt.Printf("OK: %d rule(s) loaded from %s\n", len(f.Rules), path)
			for _, r := range f.Rules {
				fmt.Printf("  - %s: action=%s max_per_run=%d\n", r.Name, r.Action, r.MaxPerRun)
			}
			return nil
		},
	}
	validate.Flags().StringVarP(&path, "config", "c", "rules.yaml", "path to rules YAML")
	c.AddCommand(validate)
	return c
}

func runCmd(resolveDir func() (string, error)) *cobra.Command {
	var (
		rulesPath   string
		dryRun      bool
		allowDelete bool
		onlyRule    string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute the rules against Gmail.",
		Long: "Runs every rule in the YAML config against Gmail. --dry-run reports matches " +
			"without changing anything. Rules with action: delete refuse to execute without " +
			"--allow-delete.",
		Example: "  # See what every rule would do without making changes\n" +
			"  gmail-cleaner run -c rules.yaml --dry-run\n" +
			"\n" +
			"  # Execute all trash / archive / label rules; delete rules are skipped\n" +
			"  gmail-cleaner run -c rules.yaml\n" +
			"\n" +
			"  # Execute everything including action: delete rules (IRREVERSIBLE)\n" +
			"  gmail-cleaner run -c rules.yaml --allow-delete\n" +
			"\n" +
			"  # Run just one named rule\n" +
			"  gmail-cleaner run -c rules.yaml --rule old-promotions",
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.Load(rulesPath)
			if err != nil {
				return err
			}
			dir, err := resolveDir()
			if err != nil {
				return err
			}
			svc, err := auth.NewService(cmd.Context(), dir)
			if err != nil {
				return err
			}
			client := gmailclient.New(svc)

			summary, runErr := engine.Run(cmd.Context(), client, f.Rules, engine.Options{
				DryRun:      dryRun,
				AllowDelete: allowDelete,
				OnlyRule:    onlyRule,
			})
			summary.Print()
			return runErr
		},
	}
	cmd.Flags().StringVarP(&rulesPath, "config", "c", "rules.yaml", "path to rules YAML")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would happen; make no changes")
	cmd.Flags().BoolVar(&allowDelete, "allow-delete", false, "permit rules with action: delete to execute (IRREVERSIBLE)")
	cmd.Flags().StringVar(&onlyRule, "rule", "", "only run the rule with this name")
	return cmd
}

func statsCmd(resolveDir func() (string, error)) *cobra.Command {
	var (
		label  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Report message counts for a label (default: INBOX).",
		Long: "Fetches counts for a label via users.labels.get. " +
			"Reads mailbox metadata only, touches no messages.",
		Example: "  gmail-cleaner stats\n" +
			"  gmail-cleaner stats --label TRASH\n" +
			"  gmail-cleaner stats --label SPAM\n" +
			"  gmail-cleaner stats --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := resolveDir()
			if err != nil {
				return err
			}
			svc, err := auth.NewService(cmd.Context(), dir)
			if err != nil {
				return err
			}
			client := gmailclient.New(svc)
			s, err := client.LabelStatsByName(cmd.Context(), label)
			if err != nil {
				return err
			}
			if asJSON {
				out := map[string]any{
					"label":          s.Name,
					"messagesTotal":  s.MessagesTotal,
					"messagesUnread": s.MessagesUnread,
					"threadsTotal":   s.ThreadsTotal,
					"threadsUnread":  s.ThreadsUnread,
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			fmt.Printf("%s: %d messages (%d unread), %d threads (%d unread)\n",
				s.Name, s.MessagesTotal, s.MessagesUnread, s.ThreadsTotal, s.ThreadsUnread)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "INBOX", "label to report on (e.g. INBOX, TRASH, SPAM)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func countCmd(resolveDir func() (string, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "count <query>",
		Short: "Count messages matching a Gmail query.",
		Long:  "Counts all messages matching a Gmail search query without touching them.",
		Example: "  gmail-cleaner count \"in:inbox is:unread\"\n" +
			"  gmail-cleaner count \"in:inbox from:revolut.com\"\n" +
			"  gmail-cleaner count --json \"in:trash\"",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			dir, err := resolveDir()
			if err != nil {
				return err
			}
			svc, err := auth.NewService(cmd.Context(), dir)
			if err != nil {
				return err
			}
			client := gmailclient.New(svc)
			n, err := client.CountMessages(cmd.Context(), query)
			if err != nil {
				return err
			}
			if asJSON {
				out := map[string]any{"query": query, "count": n}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			fmt.Printf("%d message(s) match %q\n", n, query)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func topSendersCmd(resolveDir func() (string, error)) *cobra.Command {
	var (
		query      string
		limit      int
		sampleSize int
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "top-senders",
		Short: "Show the most frequent senders in your mailbox.",
		Long: "Fetches messages matching a query, reads their From headers, and " +
			"ranks senders by message count. Useful for identifying cleanup targets.",
		Example: "  gmail-cleaner top-senders\n" +
			"  gmail-cleaner top-senders --limit 10\n" +
			"  gmail-cleaner top-senders --query \"in:inbox is:unread\"\n" +
			"  gmail-cleaner top-senders --json --sample 1000",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := resolveDir()
			if err != nil {
				return err
			}
			svc, err := auth.NewService(cmd.Context(), dir)
			if err != nil {
				return err
			}
			client := gmailclient.New(svc)

			fmt.Fprintf(os.Stderr, "Scanning up to %d messages for %q...\n", sampleSize, query)
			senders, total, err := client.TopSenders(cmd.Context(), query, sampleSize, limit)
			if err != nil {
				return err
			}
			if total == 0 {
				fmt.Println("No messages found.")
				return nil
			}

			if asJSON {
				out := map[string]any{
					"query":      query,
					"sampled":    total,
					"sampleSize": sampleSize,
					"senders":    senders,
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			fmt.Printf("Top senders in %q (%d messages sampled):\n\n", query, total)
			fmt.Printf("  %-4s  %-6s  %-5s  %s\n", "Rank", "Count", "  %", "Sender")
			fmt.Printf("  %-4s  %-6s  %-5s  %s\n", "----", "-----", "-----", "------")
			for i, s := range senders {
				pct := float64(s.Count) / float64(total) * 100
				fmt.Printf("  %-4d  %-6d  %4.1f%%  %s\n", i+1, s.Count, pct, s.Sender)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&query, "query", "in:inbox", "Gmail search query to analyze")
	cmd.Flags().IntVar(&limit, "limit", 20, "number of top senders to show")
	cmd.Flags().IntVar(&sampleSize, "sample", 500, "max messages to fetch metadata for")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func labelCmd(resolveDir func() (string, error)) *cobra.Command {
	var (
		sender string
		label  string
		dryRun bool
		maxRun int
	)
	cmd := &cobra.Command{
		Use:   "label",
		Short: "Label every message from a sender (existing + future via a Gmail filter).",
		Long: "Applies a label to all existing messages matching from:<sender> and " +
			"installs a Gmail filter so future messages from that sender are labeled " +
			"automatically. Creates the label if it doesn't exist. Idempotent: re-running " +
			"reuses the existing filter.",
		Example: "  # Preview what would happen\n" +
			"  gmail-cleaner label --from noreply@spond.com --label spond --dry-run\n" +
			"\n" +
			"  # Label existing mail AND install a filter for future mail\n" +
			"  gmail-cleaner label --from noreply@spond.com --label spond\n" +
			"\n" +
			"  # Nested labels work; created on demand\n" +
			"  gmail-cleaner label --from billing@aws.amazon.com --label \"Finance/AWS\"\n" +
			"\n" +
			"  # Cap the backfill so a huge sender doesn't dominate the run\n" +
			"  gmail-cleaner label --from noreply@github.com --label GitHub --max 500",
		RunE: func(cmd *cobra.Command, args []string) error {
			if sender == "" || label == "" {
				return fmt.Errorf("--from and --label are both required")
			}

			dir, err := resolveDir()
			if err != nil {
				return err
			}
			svc, err := auth.NewService(cmd.Context(), dir)
			if err != nil {
				return err
			}
			client := gmailclient.New(svc)
			ctx := cmd.Context()

			query := "from:" + sender
			ids, err := client.Search(ctx, query, maxRun)
			if err != nil {
				return err
			}
			slog.Info("existing matches", "sender", sender, "count", len(ids))

			if dryRun {
				fmt.Printf("[dry-run] Would label %d existing message(s) from %s with %q\n",
					len(ids), sender, label)
				fmt.Printf("[dry-run] Would create a Gmail filter: from=%s -> addLabel=%s\n",
					sender, label)
				return nil
			}

			labelID, err := client.EnsureLabel(ctx, label)
			if err != nil {
				return err
			}

			if len(ids) > 0 {
				if err := client.AddLabel(ctx, ids, labelID); err != nil {
					return err
				}
				slog.Info("labeled existing messages", "count", len(ids), "label", label)
			}

			filterID, created, err := client.CreateFilterFromSender(ctx, sender, labelID)
			if err != nil {
				return err
			}
			if created {
				slog.Info("filter installed", "id", filterID, "sender", sender, "label", label)
				fmt.Printf("Labeled %d existing message(s) and installed a filter for future mail from %s.\n",
					len(ids), sender)
			} else {
				slog.Info("filter already existed", "id", filterID, "sender", sender, "label", label)
				fmt.Printf("Labeled %d existing message(s). A matching filter already exists (id=%s).\n",
					len(ids), filterID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sender, "from", "", "sender email address to match (required)")
	cmd.Flags().StringVar(&label, "label", "", "label to apply (created if missing) (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would happen; make no changes")
	cmd.Flags().IntVar(&maxRun, "max", 0, "cap on existing messages to label (0 = no cap)")
	return cmd
}

func purgeCmd(resolveDir func() (string, error)) *cobra.Command {
	var (
		label       string
		action      string
		inTrash     bool
		all         bool
		allowDelete bool
		dryRun      bool
		maxRun      int
	)
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Trash or permanently delete all messages carrying a label.",
		Long: "Selects messages via `label:<name>` (nested paths OK). --action trash moves " +
			"them to Trash (recoverable 30 days). --action delete permanently deletes them " +
			"(irreversible; requires --allow-delete). --in-trash searches inside Trash (use " +
			"with --action delete to permanently remove already-trashed labeled messages). " +
			"--all (only valid with --in-trash --action delete) empties the entire Trash " +
			"regardless of label. Use --dry-run first.",
		Example: "  # Preview what carries the label\n" +
			"  gmail-cleaner purge --label spond --dry-run\n" +
			"\n" +
			"  # Move all messages with the label to Trash (recoverable 30 days) — default action\n" +
			"  gmail-cleaner purge --label spond\n" +
			"\n" +
			"  # Skip Trash: permanently delete labeled messages directly\n" +
			"  gmail-cleaner purge --label spond --action delete --allow-delete\n" +
			"\n" +
			"  # Purge messages ALREADY in Trash that still carry the label\n" +
			"  gmail-cleaner purge --label spond --in-trash --action delete --allow-delete\n" +
			"\n" +
			"  # Empty the entire Trash (any label, any sender)\n" +
			"  gmail-cleaner purge --all --in-trash --action delete --allow-delete --dry-run\n" +
			"  gmail-cleaner purge --all --in-trash --action delete --allow-delete\n" +
			"\n" +
			"  # Cap the number of messages touched per run\n" +
			"  gmail-cleaner purge --label spond --max 100",
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if label != "" {
					return fmt.Errorf("--all and --label are mutually exclusive")
				}
				if !inTrash || action != "delete" {
					return fmt.Errorf("--all is only allowed with --in-trash --action delete (guards against wiping the whole mailbox)")
				}
			} else if label == "" {
				return fmt.Errorf("--label is required (or use --all --in-trash --action delete to empty Trash)")
			}
			switch action {
			case "trash", "delete":
			default:
				return fmt.Errorf("--action must be 'trash' or 'delete', got %q", action)
			}
			if inTrash && action != "delete" {
				return fmt.Errorf("--in-trash only makes sense with --action delete (messages in Trash are already trashed)")
			}
			if !dryRun && action == "delete" && !allowDelete {
				return fmt.Errorf("--action delete is irreversible; pass --allow-delete to confirm")
			}

			dir, err := resolveDir()
			if err != nil {
				return err
			}
			svc, err := auth.NewService(cmd.Context(), dir)
			if err != nil {
				return err
			}
			client := gmailclient.New(svc)
			ctx := cmd.Context()

			var query, describe string
			switch {
			case all:
				query = "in:trash"
				describe = "in Trash"
			case inTrash:
				query = "in:trash label:" + quoteLabelForQuery(label)
				describe = fmt.Sprintf("in Trash with label %q", label)
			default:
				query = "label:" + quoteLabelForQuery(label)
				describe = fmt.Sprintf("with label %q", label)
			}

			ids, err := client.Search(ctx, query, maxRun)
			if err != nil {
				return err
			}
			slog.Info("matched", "query", query, "count", len(ids))

			if len(ids) == 0 {
				fmt.Printf("No messages found %s.\n", describe)
				return nil
			}

			if dryRun {
				fmt.Printf("[dry-run] Would %s %d message(s) %s\n", action, len(ids), describe)
				return nil
			}

			switch action {
			case "trash":
				if err := client.Trash(ctx, ids); err != nil {
					return err
				}
				slog.Info("trashed", "count", len(ids))
				fmt.Printf("Moved %d message(s) %s to Trash.\n", len(ids), describe)
			case "delete":
				if err := client.Delete(ctx, ids); err != nil {
					return err
				}
				slog.Info("permanently deleted", "count", len(ids), "in_trash", inTrash)
				fmt.Printf("Permanently deleted %d message(s) %s.\n", len(ids), describe)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "label whose messages to purge (required unless --all)")
	cmd.Flags().StringVar(&action, "action", "trash", "'trash' (recoverable) or 'delete' (permanent)")
	cmd.Flags().BoolVar(&inTrash, "in-trash", false, "search inside Trash (use with --action delete to purge already-trashed messages)")
	cmd.Flags().BoolVar(&all, "all", false, "empty ALL of Trash regardless of label (requires --in-trash --action delete)")
	cmd.Flags().BoolVar(&allowDelete, "allow-delete", false, "required for --action delete (IRREVERSIBLE)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would happen; make no changes")
	cmd.Flags().IntVar(&maxRun, "max", 0, "cap on messages to touch (0 = no cap)")
	return cmd
}

// quoteLabelForQuery wraps a label in double quotes if it contains a space,
// so Gmail's search parser sees a single label token.
func quoteLabelForQuery(name string) string {
	for _, r := range name {
		if r == ' ' {
			return "\"" + name + "\""
		}
	}
	return name
}

func untrashCmd(resolveDir func() (string, error)) *cobra.Command {
	var (
		label  string
		sender string
		all    bool
		dryRun bool
		maxRun int
	)
	cmd := &cobra.Command{
		Use:   "untrash",
		Short: "Restore messages from Trash back to the mailbox.",
		Long: "Undoes a trash operation. Pick messages by --label, --from, or --all. " +
			"Exactly one selector is required. Only messages currently in Trash are " +
			"affected; messages already permanently deleted cannot be recovered.",
		Example: "  # Preview what would be restored\n" +
			"  gmail-cleaner untrash --label spond --dry-run\n" +
			"\n" +
			"  # Restore everything in Trash that carries a label\n" +
			"  gmail-cleaner untrash --label spond\n" +
			"\n" +
			"  # Restore everything in Trash from a sender\n" +
			"  gmail-cleaner untrash --from noreply@spond.com\n" +
			"\n" +
			"  # Restore ALL messages currently in Trash\n" +
			"  gmail-cleaner untrash --all",
		RunE: func(cmd *cobra.Command, args []string) error {
			selectors := 0
			if label != "" {
				selectors++
			}
			if sender != "" {
				selectors++
			}
			if all {
				selectors++
			}
			if selectors != 1 {
				return fmt.Errorf("exactly one of --label, --from, or --all must be provided")
			}

			dir, err := resolveDir()
			if err != nil {
				return err
			}
			svc, err := auth.NewService(cmd.Context(), dir)
			if err != nil {
				return err
			}
			client := gmailclient.New(svc)
			ctx := cmd.Context()

			query := "in:trash"
			var describe string
			switch {
			case label != "":
				query += " label:" + quoteLabelForQuery(label)
				describe = fmt.Sprintf("with label %q", label)
			case sender != "":
				query += " from:" + sender
				describe = fmt.Sprintf("from %s", sender)
			case all:
				describe = "in Trash"
			}

			ids, err := client.Search(ctx, query, maxRun)
			if err != nil {
				return err
			}
			slog.Info("matched", "query", query, "count", len(ids))

			if len(ids) == 0 {
				fmt.Printf("No messages %s.\n", describe)
				return nil
			}

			if dryRun {
				fmt.Printf("[dry-run] Would restore %d message(s) %s\n", len(ids), describe)
				return nil
			}

			if err := client.Untrash(ctx, ids); err != nil {
				return err
			}
			slog.Info("untrashed", "count", len(ids))
			fmt.Printf("Restored %d message(s) %s from Trash.\n", len(ids), describe)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "restore messages in Trash carrying this label")
	cmd.Flags().StringVar(&sender, "from", "", "restore messages in Trash from this sender")
	cmd.Flags().BoolVar(&all, "all", false, "restore ALL messages currently in Trash")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would happen; make no changes")
	cmd.Flags().IntVar(&maxRun, "max", 0, "cap on messages to restore (0 = no cap)")
	return cmd
}
