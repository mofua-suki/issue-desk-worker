// Command review-counts aggregates selected worker completion results from a JSON array.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type counts struct {
	Full    int64 `json:"full"`
	Delta   int64 `json:"delta"`
	Skipped int64 `json:"skipped"`
}

type result struct {
	ReviewCounts *struct {
		Full    *int64 `json:"full"`
		Delta   *int64 `json:"delta"`
		Skipped *int64 `json:"skipped"`
	} `json:"reviewCounts"`
}

type report struct {
	Jobs    int    `json:"jobs"`
	Missing int    `json:"missing"`
	Counts  counts `json:"counts"`
	Ratios  *struct {
		Full    float64 `json:"full"`
		Delta   float64 `json:"delta"`
		Skipped float64 `json:"skipped"`
	} `json:"ratios"`
}

func run(in io.Reader, out io.Writer) error {
	var results []json.RawMessage
	dec := json.NewDecoder(in)
	if err := dec.Decode(&results); err != nil {
		return err
	}
	if results == nil {
		return fmt.Errorf("expected an array of completion results")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing input")
	}
	var r report
	for _, raw := range results {
		var item result
		if string(raw) == "null" {
			return fmt.Errorf("completion result must be an object")
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		if item.ReviewCounts == nil {
			r.Missing++
			continue
		}
		v := item.ReviewCounts
		if v.Full == nil || v.Delta == nil || v.Skipped == nil {
			return fmt.Errorf("incomplete reviewCounts")
		}
		c := counts{Full: *v.Full, Delta: *v.Delta, Skipped: *v.Skipped}
		if c.Full < 0 || c.Delta < 0 || c.Skipped < 0 {
			return fmt.Errorf("negative review count")
		}
		// Reject overflow instead of reporting an incorrect ratio.
		for _, pair := range []struct {
			sum *int64
			n   int64
		}{{&r.Counts.Full, c.Full}, {&r.Counts.Delta, c.Delta}, {&r.Counts.Skipped, c.Skipped}} {
			if pair.n > int64(^uint64(0)>>1)-*pair.sum {
				return fmt.Errorf("review count overflow")
			}
			*pair.sum += pair.n
		}
		r.Jobs++
	}
	f, d, s := float64(r.Counts.Full), float64(r.Counts.Delta), float64(r.Counts.Skipped)
	if n := f + d + s; n > 0 {
		r.Ratios = &struct {
			Full    float64 `json:"full"`
			Delta   float64 `json:"delta"`
			Skipped float64 `json:"skipped"`
		}{f / n, d / n, s / n}
	}
	return json.NewEncoder(out).Encode(r)
}

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
