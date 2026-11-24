package execution

import "issue-desk-worker/internal/journal"

// Capture only locally recorded sources for this target. Neither job ordering
// nor a retry marker alone authorizes dropping subsequently discovered work.
func (e *Executor) retryDiscardedSources(d Descriptor, r *journal.Record) ([]journal.ContinuationSource, error) {
	records, err := e.Store.ListIssue(d.Repo, d.Issue)
	if err != nil {
		return nil, err
	}
	discarded := append([]journal.ContinuationSource(nil), r.DiscardedContinuationSources...)
	for _, source := range records {
		if source.ID != d.ID && source.Repo == d.Repo && source.Issue == d.Issue {
			discarded = append(discarded, journal.SourceOf(source))
			if source.RetryBaseGeneration != nil {
				discarded = append(discarded, source.DiscardedContinuationSources...)
			}
		}
	}
	return discarded, nil
}

func activeContinuationSources(d Descriptor, r *journal.Record, records []journal.Record) []journal.Record {
	discarded := make(map[journal.ContinuationSource]bool)
	add := func(record journal.Record) {
		if record.RetryBaseGeneration != nil {
			for _, source := range record.DiscardedContinuationSources {
				discarded[source] = true
			}
		}
	}
	add(*r)
	for _, record := range records {
		if record.Repo == d.Repo && record.Issue == d.Issue {
			add(record)
		}
	}
	var sources []journal.Record
	for _, record := range records {
		if record.ID != d.ID && record.Repo == d.Repo && record.Issue == d.Issue && !discarded[journal.SourceOf(record)] {
			sources = append(sources, record)
		}
	}
	return sources
}

func abandonedRetryDescriptor(d Descriptor, r *journal.Record) bool {
	return explicitContinuationRetry(d, r) && d.Branch != r.Branch && (d.Branch != "" || d.PR == 0)
}
