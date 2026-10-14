// Package gmailclient is a thin, opinionated wrapper over the official
// google.golang.org/api/gmail/v1 client. It exposes only the operations the
// cleaner needs (search, archive, trash, permanent delete, label management)
// and prefers batch endpoints where they exist.
package gmailclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	gmail "google.golang.org/api/gmail/v1"
)

const (
	// batchDelete accepts up to 1000 IDs per call.
	batchDeleteMax = 1000
	// batchModify has no documented hard cap in the client but 1000 is the
	// server-side practical limit; keep it symmetric with delete.
	batchModifyMax = 1000
	// Concurrency for per-message trash calls.
	trashConcurrency = 5
)

const userMe = "me"

type Client struct {
	svc *gmail.Service
}

func New(svc *gmail.Service) *Client { return &Client{svc: svc} }

// Search returns up to `limit` message IDs matching `query` (Gmail search
// syntax). If limit <= 0, all matching messages are returned. Paginates
// automatically via nextPageToken.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]string, error) {
	var ids []string
	call := c.svc.Users.Messages.List(userMe).Q(query)
	// Ask for the largest page Gmail supports to minimize round trips.
	call.MaxResults(500)

	for {
		if limit > 0 {
			remaining := int64(limit - len(ids))
			if remaining <= 0 {
				break
			}
			if remaining < 500 {
				call.MaxResults(remaining)
			}
		}
		resp, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("messages.list %q: %w", query, err)
		}
		for _, m := range resp.Messages {
			ids = append(ids, m.Id)
			if limit > 0 && len(ids) >= limit {
				return ids, nil
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		call.PageToken(resp.NextPageToken)
	}
	return ids, nil
}

// Peek fetches minimal metadata (From + Subject) for the given IDs, used by
// dry-run to show the user what would be affected. Limited to `n` to keep
// output short.
type Peek struct {
	ID      string
	From    string
	Subject string
}

func (c *Client) PeekMetadata(ctx context.Context, ids []string, n int) ([]Peek, error) {
	if n > len(ids) {
		n = len(ids)
	}
	out := make([]Peek, 0, n)
	for _, id := range ids[:n] {
		m, err := c.svc.Users.Messages.Get(userMe, id).
			Format("metadata").
			MetadataHeaders("From", "Subject").
			Context(ctx).Do()
		if err != nil {
			return out, fmt.Errorf("messages.get %s: %w", id, err)
		}
		p := Peek{ID: id}
		for _, h := range m.Payload.Headers {
			switch h.Name {
			case "From":
				p.From = h.Value
			case "Subject":
				p.Subject = h.Value
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Archive removes the INBOX label from all given messages in batches.
func (c *Client) Archive(ctx context.Context, ids []string) error {
	return c.batchModify(ctx, ids, nil, []string{"INBOX"})
}

// AddLabel adds the given label ID to all messages.
func (c *Client) AddLabel(ctx context.Context, ids []string, labelID string) error {
	return c.batchModify(ctx, ids, []string{labelID}, nil)
}

func (c *Client) batchModify(ctx context.Context, ids, add, remove []string) error {
	for start := 0; start < len(ids); start += batchModifyMax {
		end := start + batchModifyMax
		if end > len(ids) {
			end = len(ids)
		}
		req := &gmail.BatchModifyMessagesRequest{
			Ids:            ids[start:end],
			AddLabelIds:    add,
			RemoveLabelIds: remove,
		}
		if err := c.svc.Users.Messages.BatchModify(userMe, req).Context(ctx).Do(); err != nil {
			return fmt.Errorf("batchModify [%d:%d]: %w", start, end, err)
		}
	}
	return nil
}

// Trash moves messages to Trash. The Gmail API has no batch trash endpoint,
// so this fans out with bounded concurrency.
func (c *Client) Trash(ctx context.Context, ids []string) error {
	return c.perMessageConcurrent(ctx, ids, "trash", func(ctx context.Context, id string) error {
		_, err := c.svc.Users.Messages.Trash(userMe, id).Context(ctx).Do()
		return err
	})
}

// Untrash restores messages from Trash back to their prior labels. Also no
// batch endpoint, so fan-out with the same pattern as Trash.
func (c *Client) Untrash(ctx context.Context, ids []string) error {
	return c.perMessageConcurrent(ctx, ids, "untrash", func(ctx context.Context, id string) error {
		_, err := c.svc.Users.Messages.Untrash(userMe, id).Context(ctx).Do()
		return err
	})
}

func (c *Client) perMessageConcurrent(ctx context.Context, ids []string, op string, fn func(context.Context, string) error) error {
	sem := make(chan struct{}, trashConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, id); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%s %s: %w", op, id, err)
				}
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	return firstErr
}

// Delete permanently removes messages. IRREVERSIBLE.
func (c *Client) Delete(ctx context.Context, ids []string) error {
	for start := 0; start < len(ids); start += batchDeleteMax {
		end := start + batchDeleteMax
		if end > len(ids) {
			end = len(ids)
		}
		req := &gmail.BatchDeleteMessagesRequest{Ids: ids[start:end]}
		if err := c.svc.Users.Messages.BatchDelete(userMe, req).Context(ctx).Do(); err != nil {
			return fmt.Errorf("batchDelete [%d:%d]: %w", start, end, err)
		}
	}
	return nil
}

// EnsureLabel returns the label ID for `name`, creating the label (including
// nested parents via `/`-separated names, which Gmail interprets natively)
// if it doesn't exist. System labels like INBOX are returned as-is.
func (c *Client) EnsureLabel(ctx context.Context, name string) (string, error) {
	if id, ok := systemLabelID(name); ok {
		return id, nil
	}
	resp, err := c.svc.Users.Labels.List(userMe).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("labels.list: %w", err)
	}
	for _, l := range resp.Labels {
		if strings.EqualFold(l.Name, name) {
			return l.Id, nil
		}
	}
	created, err := c.svc.Users.Labels.Create(userMe, &gmail.Label{
		Name:                  name,
		LabelListVisibility:   "labelShow",
		MessageListVisibility: "show",
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("labels.create %q: %w", name, err)
	}
	return created.Id, nil
}

func systemLabelID(name string) (string, bool) {
	up := strings.ToUpper(name)
	switch up {
	case "INBOX", "SPAM", "TRASH", "UNREAD", "STARRED", "IMPORTANT", "SENT", "DRAFT":
		return up, true
	}
	return "", false
}

// LabelStats holds counts returned by users.labels.get for a single label.
type LabelStats struct {
	Name           string
	MessagesTotal  int64
	MessagesUnread int64
	ThreadsTotal   int64
	ThreadsUnread  int64
}

// InboxStats returns totals for the system INBOX label via a single
// users.labels.get call. Much cheaper than paginating a search.
func (c *Client) InboxStats(ctx context.Context) (LabelStats, error) {
	l, err := c.svc.Users.Labels.Get(userMe, "INBOX").Context(ctx).Do()
	if err != nil {
		return LabelStats{}, fmt.Errorf("labels.get INBOX: %w", err)
	}
	return LabelStats{
		Name:           l.Name,
		MessagesTotal:  l.MessagesTotal,
		MessagesUnread: l.MessagesUnread,
		ThreadsTotal:   l.ThreadsTotal,
		ThreadsUnread:  l.ThreadsUnread,
	}, nil
}

// CreateFilterFromSender installs a Gmail filter that adds `labelID` to any
// future incoming message whose From matches `sender`. Returns the created
// filter's ID.
//
// If a functionally-identical filter already exists (same From + same
// addLabelIds), returns that filter's ID and ok=false to indicate no
// creation happened. This makes the command idempotent.
func (c *Client) CreateFilterFromSender(ctx context.Context, sender, labelID string) (id string, created bool, err error) {
	existing, err := c.svc.Users.Settings.Filters.List(userMe).Context(ctx).Do()
	if err != nil {
		return "", false, fmt.Errorf("filters.list: %w", err)
	}
	for _, f := range existing.Filter {
		if f.Criteria == nil || f.Action == nil {
			continue
		}
		if !strings.EqualFold(f.Criteria.From, sender) {
			continue
		}
		for _, id := range f.Action.AddLabelIds {
			if id == labelID {
				return f.Id, false, nil
			}
		}
	}

	filter := &gmail.Filter{
		Criteria: &gmail.FilterCriteria{From: sender},
		Action:   &gmail.FilterAction{AddLabelIds: []string{labelID}},
	}
	created2, err := c.svc.Users.Settings.Filters.Create(userMe, filter).Context(ctx).Do()
	if err != nil {
		return "", false, fmt.Errorf("filters.create: %w", err)
	}
	return created2.Id, true, nil
}

// CountMessages returns the number of messages matching a Gmail query.
func (c *Client) CountMessages(ctx context.Context, query string) (int, error) {
	ids, err := c.Search(ctx, query, 0)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// LabelStatsByName returns counts for any label (system or user-created).
func (c *Client) LabelStatsByName(ctx context.Context, name string) (LabelStats, error) {
	labelID := name
	if id, ok := systemLabelID(name); ok {
		labelID = id
	}
	l, err := c.svc.Users.Labels.Get(userMe, labelID).Context(ctx).Do()
	if err != nil {
		return LabelStats{}, fmt.Errorf("labels.get %s: %w", name, err)
	}
	return LabelStats{
		Name:           l.Name,
		MessagesTotal:  l.MessagesTotal,
		MessagesUnread: l.MessagesUnread,
		ThreadsTotal:   l.ThreadsTotal,
		ThreadsUnread:  l.ThreadsUnread,
	}, nil
}

// SenderCount holds an aggregated sender and its message count.
type SenderCount struct {
	Sender string
	Count  int
}

// TopSenders returns the most frequent senders for messages matching query.
// It fetches metadata for up to sampleSize messages and returns the top limit senders.
func (c *Client) TopSenders(ctx context.Context, query string, sampleSize, limit int) ([]SenderCount, int, error) {
	ids, err := c.Search(ctx, query, sampleSize)
	if err != nil {
		return nil, 0, err
	}
	total := len(ids)
	if total == 0 {
		return nil, 0, nil
	}

	peeks, err := c.PeekMetadata(ctx, ids, total)
	if err != nil {
		return nil, total, err
	}

	counts := make(map[string]int)
	for _, p := range peeks {
		counts[p.From]++
	}

	sorted := make([]SenderCount, 0, len(counts))
	for sender, count := range counts {
		sorted = append(sorted, SenderCount{Sender: sender, Count: count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Count > sorted[j].Count
	})

	if limit > 0 && limit < len(sorted) {
		sorted = sorted[:limit]
	}
	return sorted, total, nil
}
