package gitea

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PageSize is the limit sent with every listing. Gitea caps limit at
// MAX_RESPONSE_ITEMS (50 by default).
const PageSize = 50

// ErrListIncomplete reports a listing that did not reach its last page within the page bound.
var ErrListIncomplete = errors.New("Giteaの一覧を最後まで読めません")

// Each reads the paged listing at path (without limit/page) and calls visit for
// every row in order until visit reports done or returns an error.
//
// An empty page ends the listing. A page shorter than PageSize ends it too,
// unless the X-Total-Count header says more rows remain (a server whose
// MAX_RESPONSE_ITEMS is below PageSize returns short pages that are not the
// last). A full page always reads the next one, so a stale count never cuts
// the listing short. After maxPages pages it returns ErrListIncomplete.
func Each[T any](ctx context.Context, c *Client, path string, maxPages int, visit func(T) (done bool, err error)) error {
	return each(ctx, c, path, maxPages, false, visit)
}

// EachRequired rejects null in place of an array, including an empty listing.
func EachRequired[T any](ctx context.Context, c *Client, path string, maxPages int, visit func(T) (done bool, err error)) error {
	return each(ctx, c, path, maxPages, true, visit)
}

func each[T any](ctx context.Context, c *Client, path string, maxPages int, required bool, visit func(T) (done bool, err error)) error {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	seen := 0
	for page := 1; page <= maxPages; page++ {
		var rows []T
		header, err := c.do(ctx, "GET", fmt.Sprintf("%s%slimit=%d&page=%d", path, separator, PageSize, page), nil, &rows)
		if err != nil {
			return err
		}
		if required && rows == nil {
			return ErrListIncomplete
		}
		for _, row := range rows {
			if done, err := visit(row); err != nil || done {
				return err
			}
		}
		seen += len(rows)
		total, err := strconv.Atoi(header.Get("X-Total-Count"))
		known := err == nil && total >= 0
		if len(rows) == 0 || len(rows) < PageSize && (!known || seen >= total) {
			return nil
		}
	}
	return ErrListIncomplete
}

// EachPull reads the repository's pull requests in the given state (open, closed, all).
func (c *Client) EachPull(ctx context.Context, repo, state string, maxPages int, visit func(Pull) (done bool, err error)) error {
	return Each(ctx, c, fmt.Sprintf("/repos/%s/pulls?state=%s", repo, state), maxPages, visit)
}
