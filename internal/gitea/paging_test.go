package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// pagingServer serves rows 1..rows, at most capPerPage per page; header=false
// omits X-Total-Count. It counts the pages read.
func pagingServer(t *testing.T, rows, capPerPage, reportedTotal int, header bool) (*Client, *int) {
	t.Helper()
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit > capPerPage {
			limit = capPerPage
		}
		out := []int{}
		for i := (page-1)*limit + 1; i <= page*limit && i <= rows; i++ {
			out = append(out, i)
		}
		if header {
			w.Header().Set("X-Total-Count", strconv.Itoa(reportedTotal))
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(server.Close)
	c, err := New(server.URL, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	return c, &pages
}

func collect(t *testing.T, c *Client, maxPages int) ([]int, error) {
	t.Helper()
	var got []int
	err := Each(context.Background(), c, "/items", maxPages, func(v int) (bool, error) { got = append(got, v); return false, nil })
	return got, err
}

func TestEachPaging(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		rows, capPerPage, reported int
		header                     bool
		maxPages, want, wantPages  int
		incomplete                 bool
	}{
		{"件数の見出しが無く満杯の次が空", 50, 50, 0, false, 5, 50, 2, false},
		{"件数の見出しが無く短いページで終わる", 60, 50, 0, false, 5, 60, 2, false},
		{"上限が50未満でも件数まで読む", 31, 30, 31, true, 5, 31, 2, false},
		{"件数が実際より少なくても満杯なら次を読む", 120, 50, 60, true, 5, 120, 3, false},
		{"上限のページ数で読み終わらなければ不完全", 200, 50, 200, true, 2, 100, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, pages := pagingServer(t, tc.rows, tc.capPerPage, tc.reported, tc.header)
			got, err := collect(t, c, tc.maxPages)
			if tc.incomplete != errors.Is(err, ErrListIncomplete) || !tc.incomplete && err != nil {
				t.Fatalf("err=%v", err)
			}
			if len(got) != tc.want || *pages != tc.wantPages {
				t.Fatalf("rows=%d pages=%d, want rows=%d pages=%d", len(got), *pages, tc.want, tc.wantPages)
			}
		})
	}
}

// visit が終わりを返したら、その先のページを読まない。
func TestEachStopsWhenVisitIsDone(t *testing.T) {
	c, pages := pagingServer(t, 120, 50, 120, true)
	seen := 0
	err := Each(context.Background(), c, "/items", 5, func(v int) (bool, error) { seen++; return v == 10, nil })
	if err != nil || seen != 10 || *pages != 1 {
		t.Fatalf("err=%v seen=%d pages=%d", err, seen, *pages)
	}
}
